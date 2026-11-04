// Package money formats amounts of money. Amounts are int64 dong everywhere
// (SPEC.md 3.1); this package only turns them into text for people.
package money

import "strconv"

// FormatVND groups thousands with dots, Vietnamese style: 7000000 -> "7.000.000".
func FormatVND(n int64) string {
	sign, abs := "", uint64(n)
	if n < 0 {
		// Negating in uint64 also covers math.MinInt64, which has no int64
		// opposite.
		sign, abs = "-", -uint64(n)
	}
	return sign + group(strconv.FormatUint(abs, 10))
}

func group(s string) string {
	out := make([]byte, 0, len(s)+len(s)/3)
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, '.')
		}
		out = append(out, s[i])
	}
	return string(out)
}
