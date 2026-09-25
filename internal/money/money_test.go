package money

import "testing"

func TestFormatVND(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0", 999: "999", 1000: "1.000", 3500000: "3.500.000", 12345678: "12.345.678", -1500: "-1.500",
	} {
		if got := FormatVND(n); got != want {
			t.Errorf("FormatVND(%d) = %s, want %s", n, got, want)
		}
	}
}
