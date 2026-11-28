package main

import (
	"bytes"
	"database/sql"
	"strings"
	"testing"
)

// parseFrame follows firmware Packet::readFrom: reserved hash size, path
// cap, non-empty payload within MAX_PACKET_PAYLOAD.
func TestParseFrame(t *testing.T) {
	ok := []struct {
		name                  string
		hex                   string
		route, size, path, pl int
		pt                    byte
	}{
		{"flood 1-byte", frameHex(1, 5, 1, []byte{1, 2}, []byte{9}), 1, 1, 2, 1, 5},
		{"transport flood 2-byte", frameHex(0, 4, 2, []byte{1, 2, 3, 4}, []byte{9, 9}), 0, 2, 4, 2, 4},
		{"transport direct 3-byte", frameHex(3, 2, 3, []byte{1, 2, 3}, []byte{9}), 3, 3, 3, 1, 2},
		{"zero hops", frameHex(1, 5, 1, nil, []byte{9}), 1, 1, 0, 1, 5},
		{"64 path bytes, 184 payload", frameHex(1, 5, 1, make([]byte, 63), make([]byte, 184)), 1, 1, 63, 184, 5},
		{"lower case hex", strings.ToLower(frameHex(2, 2, 1, []byte{0xab}, []byte{9})), 2, 1, 1, 1, 2},
	}
	for _, c := range ok {
		f, err := parseFrame(c.hex)
		if err != nil || f.route != c.route || f.hashSize != c.size || len(f.path) != c.path || len(f.payload) != c.pl || f.payloadType != c.pt {
			t.Fatalf("%s: %+v %v", c.name, f, err)
		}
	}
	bad := map[string]string{
		"not hex":            "ZZ",
		"one byte":           "15",
		"transport no path":  "1012340000",
		"reserved hash size": "15C1AABBCCDD09", // size 4, one hop, payload 09
		"no payload":         frameHex(1, 5, 1, []byte{1, 2}, nil),
		"path past end":      "1503AA09",
		"payload over 184":   frameHex(1, 5, 1, nil, make([]byte, 185)),
		"path over 64 bytes": "15" + "62" + strings.Repeat("00", 2*34+1), // 2-byte hops x 34 = 68
	}
	for name, h := range bad {
		if _, err := parseFrame(h); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestParsePathJSON(t *testing.T) {
	ns := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	for _, c := range []struct {
		in    sql.NullString
		width int
		hops  []byte
	}{
		{ns(`[]`), 0, nil},
		{ns(`["0B","01"]`), 1, []byte{0x0b, 0x01}},
		{ns(`["fd12","0001"]`), 2, []byte{0xfd, 0x12, 0x00, 0x01}},
		{ns(`["AABBCC"]`), 3, []byte{0xaa, 0xbb, 0xcc}},
	} {
		w, h, ok := parsePathJSON(c.in)
		if !ok || w != c.width || !bytes.Equal(h, c.hops) {
			t.Fatalf("%q: %d %x %v", c.in.String, w, h, ok)
		}
	}
	long := `["` + strings.Repeat(`00","`, 64) + `00"]` // 65 one-byte hops
	for _, in := range []sql.NullString{{}, ns(""), ns(`{}`), ns(`["0B","0102"]`), ns(`["01020304"]`),
		ns(`["XY"]`), ns(`[""]`), ns(`[1]`), ns(long)} {
		if _, _, ok := parsePathJSON(in); ok {
			t.Fatalf("%q accepted", in.String)
		}
	}
}
