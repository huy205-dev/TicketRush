package catalog

import "testing"

func TestDemoEventLayout(t *testing.T) {
	seats := DemoEvent().Seats()
	if len(seats) != 5000 {
		t.Fatalf("got %d seats, want 5000", len(seats))
	}

	type zoneStat struct {
		count int
		price int64
	}
	got := map[string]zoneStat{}
	ids := map[string]bool{}
	for _, s := range seats {
		if ids[s.ID] {
			t.Fatalf("duplicate seat id %s", s.ID)
		}
		ids[s.ID] = true
		z := got[s.Zone]
		z.count++
		if z.price != 0 && z.price != s.PriceVND {
			t.Errorf("zone %s has mixed prices %d and %d", s.Zone, z.price, s.PriceVND)
		}
		z.price = s.PriceVND
		got[s.Zone] = z
	}

	want := map[string]zoneStat{
		"VIP":  {500, 3_500_000},
		"CAT1": {1500, 2_000_000},
		"CAT2": {3000, 1_000_000},
	}
	for zone, w := range want {
		if got[zone] != w {
			t.Errorf("zone %s = %+v, want %+v", zone, got[zone], w)
		}
	}
	for _, id := range []string{"VIP-A-1", "VIP-J-50", "CAT1-O-100", "CAT2-T-150"} {
		if !ids[id] {
			t.Errorf("expected seat %s to exist", id)
		}
	}
}

func TestRowLabel(t *testing.T) {
	for i, want := range map[int]string{0: "A", 1: "B", 25: "Z", 26: "AA", 27: "AB", 51: "AZ", 52: "BA"} {
		if got := rowLabel(i); got != want {
			t.Errorf("rowLabel(%d) = %q, want %q", i, got, want)
		}
	}
}
