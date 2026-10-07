package packetpath

import "testing"

func TestSenderHashSize(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      int
	}{
		{"flood one byte", "1502AABBDEADBEEF", 1},
		{"flood two bytes at zero hops", "1540DEADBEEF", 2},
		{"flood three bytes at zero hops", "1580DEADBEEF", 3},
		{"transport flood", "141122334480DEADBEEF", 3},
		{"direct with path", "1641AABBDEADBEEF", 2},
		{"direct zero hop", "1600DEADBEEF", 0},
		{"transport direct zero hop", "171122334400DEADBEEF", 0},
		{"trace header is SNR", "2542AABBDEADBEEF", 0},
		{"reserved size", "15C0DEADBEEF", 0},
		{"missing path byte", "15", 0},
		{"truncated transport header", "141122", 0},
		{"bad hex", "ZZ40", 0},
		{"empty", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SenderHashSize(tc.raw); got != tc.want {
				t.Errorf("SenderHashSize(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}
