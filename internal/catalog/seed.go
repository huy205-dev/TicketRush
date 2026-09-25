package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/huy205-dev/ticketrush/internal/catalog/catalogdb"
)

// ZoneLayout describes a rectangular block of seats. Rows are labelled A, B,
// C... and seats are numbered from 1, so the first seat is "<Zone>-A-1".
type ZoneLayout struct {
	Zone        string
	Rows        int
	SeatsPerRow int
	PriceVND    int64
}

// EventSpec is what Seed creates. Times are offsets from the database clock
// so a freshly seeded event can be on sale immediately.
type EventSpec struct {
	Name             string
	Venue            string
	StartsIn         time.Duration
	SaleOpensIn      time.Duration
	MaxSeatsPerOrder int32
	Zones            []ZoneLayout
}

// DemoEvent is the 5,000-seat event from SPEC.md M1: VIP 500 seats at
// 3,500,000 đ, CAT1 1,500 at 2,000,000 đ and CAT2 3,000 at 1,000,000 đ.
func DemoEvent() EventSpec {
	return EventSpec{
		Name:             "TicketRush Live 2026",
		Venue:            "Sân vận động Mỹ Đình",
		StartsIn:         30 * 24 * time.Hour,
		SaleOpensIn:      0,
		MaxSeatsPerOrder: 4,
		Zones: []ZoneLayout{
			{Zone: "VIP", Rows: 10, SeatsPerRow: 50, PriceVND: 3_500_000},
			{Zone: "CAT1", Rows: 15, SeatsPerRow: 100, PriceVND: 2_000_000},
			{Zone: "CAT2", Rows: 20, SeatsPerRow: 150, PriceVND: 1_000_000},
		},
	}
}

// Seats expands the layout into individual seats.
func (s EventSpec) Seats() []Seat {
	var out []Seat
	for _, z := range s.Zones {
		for r := 0; r < z.Rows; r++ {
			row := rowLabel(r)
			for n := 1; n <= z.SeatsPerRow; n++ {
				out = append(out, Seat{
					ID:       fmt.Sprintf("%s-%s-%d", z.Zone, row, n),
					Zone:     z.Zone,
					Row:      row,
					No:       n,
					PriceVND: z.PriceVND,
				})
			}
		}
	}
	return out
}

// rowLabel maps 0 to A, 25 to Z, 26 to AA, like spreadsheet columns.
func rowLabel(i int) string {
	label := ""
	for i++; i > 0; i = (i - 1) / 26 {
		label = string(rune('A'+(i-1)%26)) + label
	}
	return label
}

// TxBeginner is satisfied by *pgxpool.Pool and *pgx.Conn.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Seed inserts the event and its seats in one transaction and returns the
// new event id.
func Seed(ctx context.Context, db TxBeginner, spec EventSpec) (int64, error) {
	var eventID int64
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		q := catalogdb.New(tx)
		var err error
		eventID, err = q.CreateEvent(ctx, catalogdb.CreateEventParams{
			Name:             spec.Name,
			Venue:            spec.Venue,
			StartsIn:         spec.StartsIn,
			SaleOpensIn:      spec.SaleOpensIn,
			MaxSeatsPerOrder: spec.MaxSeatsPerOrder,
		})
		if err != nil {
			return fmt.Errorf("create event: %w", err)
		}

		seats := spec.Seats()
		rows := make([]catalogdb.InsertSeatsParams, len(seats))
		for i, st := range seats {
			rows[i] = catalogdb.InsertSeatsParams{
				EventID:  eventID,
				SeatID:   st.ID,
				Zone:     st.Zone,
				RowLabel: st.Row,
				SeatNo:   int32(st.No),
				PriceVnd: st.PriceVND,
			}
		}
		n, err := q.InsertSeats(ctx, rows)
		if err != nil {
			return fmt.Errorf("insert seats: %w", err)
		}
		if int(n) != len(seats) {
			return fmt.Errorf("insert seats: copied %d of %d rows", n, len(seats))
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("seed event %q: %w", spec.Name, err)
	}
	return eventID, nil
}
