// Package catalog serves events and their seat layout.
//
// Events and seats never change after seeding (there is no admin API), so
// they are loaded from PostgreSQL once per process and kept in memory.
// Restart the services after re-seeding with a different layout.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"github.com/huy205-dev/ticketrush/internal/catalog/catalogdb"
	"github.com/huy205-dev/ticketrush/internal/inventory"
)

// ErrEventNotFound is returned for an unknown event id.
var ErrEventNotFound = errors.New("event not found")

// Event is an event with its full seat layout.
type Event struct {
	ID               int64
	Name             string
	Venue            string
	StartsAt         time.Time
	SaleOpensAt      time.Time
	MaxSeatsPerOrder int
	Zones            []Zone
	Seats            []Seat // display order: zone by price desc, row, seat number

	seatIDs []string
	byID    map[string]int
}

// Seat is one numbered seat.
type Seat struct {
	ID       string
	Zone     string
	Row      string
	No       int
	PriceVND int64
}

// Zone summarises the seats of one zone.
type Zone struct {
	Name     string
	PriceVND int64
	Seats    int
}

// Seat looks up a seat by id.
func (e *Event) Seat(id string) (Seat, bool) {
	i, ok := e.byID[id]
	if !ok {
		return Seat{}, false
	}
	return e.Seats[i], true
}

// SeatIDs returns every seat id in display order. Callers must not modify it.
func (e *Event) SeatIDs() []string { return e.seatIDs }

// setSeats installs seats (already in display order) and builds the zone
// summary and lookup index.
func (e *Event) setSeats(seats []Seat) {
	e.Seats = seats
	e.Zones = nil
	e.seatIDs = make([]string, len(seats))
	e.byID = make(map[string]int, len(seats))
	for i, st := range seats {
		e.seatIDs[i] = st.ID
		e.byID[st.ID] = i
		if n := len(e.Zones); n == 0 || e.Zones[n-1].Name != st.Zone || e.Zones[n-1].PriceVND != st.PriceVND {
			e.Zones = append(e.Zones, Zone{Name: st.Zone, PriceVND: st.PriceVND})
		}
		e.Zones[len(e.Zones)-1].Seats++
	}
}

// NewTestEvent builds an in-memory event from spec without a database, for
// tests in other packages. Zones must be listed in display order.
func NewTestEvent(id int64, spec EventSpec) *Event {
	ev := &Event{ID: id, Name: spec.Name, Venue: spec.Venue, MaxSeatsPerOrder: int(spec.MaxSeatsPerOrder)}
	ev.setSeats(spec.Seats())
	return ev
}

// Service loads events lazily and caches them for the life of the process.
type Service struct {
	q   *catalogdb.Queries
	inv inventory.Inventory

	mu       sync.RWMutex
	events   map[int64]*Event
	saleOpen map[int64]bool // only ever set to true: time does not go backwards
	loads    singleflight.Group
}

// NewService returns a catalog backed by db, reading seat status from inv.
func NewService(db catalogdb.DBTX, inv inventory.Inventory) *Service {
	return &Service{
		q:        catalogdb.New(db),
		inv:      inv,
		events:   make(map[int64]*Event),
		saleOpen: make(map[int64]bool),
	}
}

// Event returns the event with its seats, loading it on first use.
// Concurrent first requests share a single database load.
func (s *Service) Event(ctx context.Context, id int64) (*Event, error) {
	s.mu.RLock()
	ev, ok := s.events[id]
	s.mu.RUnlock()
	if ok {
		return ev, nil
	}

	v, err, _ := s.loads.Do(strconv.FormatInt(id, 10), func() (any, error) {
		// Detach from the first caller so its cancellation does not fail the
		// other callers waiting on the same load.
		ev, err := s.load(context.WithoutCancel(ctx), id)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.events[id] = ev
		s.mu.Unlock()
		return ev, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*Event), nil
}

func (s *Service) load(ctx context.Context, id int64) (*Event, error) {
	row, err := s.q.GetEvent(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %d", ErrEventNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("load event %d: %w", id, err)
	}
	seats, err := s.q.ListSeats(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load seats of event %d: %w", id, err)
	}

	layout := make([]Seat, len(seats))
	for i, st := range seats {
		layout[i] = Seat{ID: st.SeatID, Zone: st.Zone, Row: st.RowLabel, No: int(st.SeatNo), PriceVND: st.PriceVnd}
	}
	ev := &Event{
		ID:               row.ID,
		Name:             row.Name,
		Venue:            row.Venue,
		StartsAt:         row.StartsAt,
		SaleOpensAt:      row.SaleOpensAt,
		MaxSeatsPerOrder: int(row.MaxSeatsPerOrder),
	}
	ev.setSeats(layout)
	return ev, nil
}

// SaleOpen reports whether the sale has started according to the database
// clock. Once open, the answer is cached and no longer queried.
func (s *Service) SaleOpen(ctx context.Context, eventID int64) (bool, error) {
	s.mu.RLock()
	open := s.saleOpen[eventID]
	s.mu.RUnlock()
	if open {
		return true, nil
	}

	open, err := s.q.IsSaleOpen(ctx, eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("%w: %d", ErrEventNotFound, eventID)
	}
	if err != nil {
		return false, fmt.Errorf("check sale open for event %d: %w", eventID, err)
	}
	if open {
		s.mu.Lock()
		s.saleOpen[eventID] = true
		s.mu.Unlock()
	}
	return open, nil
}

// SeatMap is the seat layout with the current status of every seat.
type SeatMap struct {
	// Version increases whenever a seat changes state. Backends without such
	// a counter (pg) report 0 and Versioned is false.
	Version   int64
	Versioned bool
	Seats   []SeatState
}

// SeatState is a seat with its status.
type SeatState struct {
	Seat
	Status inventory.SeatStatus
}

// SeatMap returns every seat of the event with its current status.
func (s *Service) SeatMap(ctx context.Context, eventID int64) (*SeatMap, error) {
	ev, err := s.Event(ctx, eventID)
	if err != nil {
		return nil, err
	}
	// Read the version first: if a seat changes between the two reads the
	// map is newer than its version and the next poll refreshes again, which
	// is harmless. The other order could hide a change behind a new version.
	var version int64
	v, versioned := s.inv.(inventory.Versioner)
	if versioned {
		if version, err = v.SeatMapVersion(ctx, eventID); err != nil {
			return nil, fmt.Errorf("seat map version of event %d: %w", eventID, err)
		}
	}
	status, err := s.inv.Status(ctx, eventID, ev.SeatIDs())
	if err != nil {
		return nil, fmt.Errorf("seat map of event %d: %w", eventID, err)
	}
	m := &SeatMap{Version: version, Versioned: versioned, Seats: make([]SeatState, len(ev.Seats))}
	for i, st := range ev.Seats {
		m.Seats[i] = SeatState{Seat: st, Status: status[st.ID]}
	}
	return m, nil
}
