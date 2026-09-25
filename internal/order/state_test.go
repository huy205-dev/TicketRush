package order

import (
	"fmt"
	"testing"
)

// TestCanTransitionEveryPair checks all 7x7 status pairs against the state
// diagram in SPEC.md section 8, written out independently of the map in
// state.go.
func TestCanTransitionEveryPair(t *testing.T) {
	allowed := map[[2]Status]bool{
		{StatusHeld, StatusPaid}:           true,
		{StatusHeld, StatusCancelled}:      true,
		{StatusHeld, StatusExpired}:        true,
		{StatusHeld, StatusRefunding}:      true,
		{StatusPaid, StatusTicketed}:       true,
		{StatusExpired, StatusPaid}:        true,
		{StatusExpired, StatusRefunding}:   true,
		{StatusCancelled, StatusRefunding}: true,
		{StatusRefunding, StatusRefunded}:  true,
	}

	pairs := 0
	for _, from := range AllStatuses {
		for _, to := range AllStatuses {
			pairs++
			t.Run(fmt.Sprintf("%s->%s", from, to), func(t *testing.T) {
				if got, want := CanTransition(from, to), allowed[[2]Status{from, to}]; got != want {
					t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
				}
			})
		}
	}
	if pairs != 49 {
		t.Fatalf("checked %d pairs, want 49", pairs)
	}
}

func TestStateMachineCoversAllStatuses(t *testing.T) {
	if len(transitions) != len(AllStatuses) {
		t.Errorf("transitions has %d entries, want one per status (%d)", len(transitions), len(AllStatuses))
	}
	for _, s := range AllStatuses {
		if _, ok := transitions[s]; !ok {
			t.Errorf("status %s missing from transitions", s)
		}
	}
}

func TestUnknownStatusCannotTransition(t *testing.T) {
	if CanTransition("BOGUS", StatusPaid) || CanTransition(StatusHeld, "BOGUS") {
		t.Error("unknown statuses must not transition")
	}
}
