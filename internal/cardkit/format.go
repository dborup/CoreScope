package cardkit

import (
	"math"
	"strconv"
	"strings"
)

// FmtCount writes v with a comma between thousands, as toLocaleString
// does in English.
func FmtCount(v int) string {
	s := strconv.Itoa(v)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && s[i-1] != '-' && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// ToFixed1 is JS Number.prototype.toFixed(1) for a value ≥ 0: the exact
// binary value rounded to one decimal, an exact tie (x.25, x.75) rounded
// up, where Go's formatter would round it to even.
func ToFixed1(v float64) string {
	if q := float64(v * 4); q == math.Trunc(q) && float64(v*2) != math.Trunc(float64(v*2)) {
		v = math.Ceil(float64(v*10)) / 10
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}
