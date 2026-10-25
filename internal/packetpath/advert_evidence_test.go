package packetpath

import (
	"strings"
	"testing"
)

func verified(k AdvertKind, hops int) AdvertHearing {
	return AdvertHearing{Kind: k, Hops: hops, Verified: true}
}

// #488 M1: every route type, both transport variants' 4-byte offset, hash
// sizes, the contact re-share and the encodings Packet::readFrom rejects.
// Ported from the research prototype in draft PR #601.
func TestClassifyAdvertFrame(t *testing.T) {
	const pl = "aabbccddeeff0011" // stand-in payload bytes
	for _, tc := range []struct {
		name, raw string
		want      AdvertHearing
	}{
		{"flood zero-hop", "1100" + pl, verified(AdvertDirect, 0)},
		{"flood zero-hop 2-byte hashes", "1140" + pl, verified(AdvertDirect, 0)},
		{"flood 3 hops 1-byte", "1103a1a2a3" + pl, verified(AdvertRelayed, 3)},
		{"flood 2 hops 2-byte", "1142a1a1a2a2" + pl, verified(AdvertRelayed, 2)},
		{"flood 1 hop 3-byte", "1181a1a1a1" + pl, verified(AdvertRelayed, 1)},
		// Transport routes: offset 1 holds the first code byte (0x1a reads as
		// 26 one-byte hops), the path byte is at offset 5.
		{"transport flood zero-hop", "101a2b3c4d00" + pl, verified(AdvertDirect, 0)},
		{"transport flood 2 hops", "101a2b3c4d02a1a2" + pl, verified(AdvertRelayed, 2)},
		{"transport flood zero codes zero-hop", "100000000000" + pl, verified(AdvertDirect, 0)},
		{"direct zero-hop", "1200" + pl, verified(AdvertDirect, 0)},
		{"direct zero-hop 2-byte mode", "1240" + pl, verified(AdvertDirect, 0)},
		{"direct with a path (hops to go, not travelled)", "1202a1a2" + pl, verified(AdvertUnknown, 0)},
		{"transport direct, codes {0,0}: shared contact", "130000000000" + pl, verified(AdvertShared, 0)},
		// Byte 1 is 0xC0: read as a path byte it would be the reserved
		// hash size; at the real offset (5) the path byte is 0x00.
		{"transport direct zero-hop, non-zero codes", "13c000000000" + pl, verified(AdvertUnknown, 0)},
		{"transport direct, last code byte non-zero", "130000000100" + pl, verified(AdvertUnknown, 0)},
		{"transport direct with path", "130000000001a1" + pl, verified(AdvertUnknown, 0)},
		{"reserved hash size", "11c0" + pl, verified(AdvertUnknown, 0)},
		{"path over MAX_PATH_SIZE", "117f" + strings.Repeat("a1", 126) + pl, verified(AdvertUnknown, 0)},
		{"63 one-byte hops is valid", "113f" + strings.Repeat("a1", 63) + pl, verified(AdvertRelayed, 63)},
		{"32 two-byte hops is valid", "1160" + strings.Repeat("a1a2", 32) + pl, verified(AdvertRelayed, 32)},
		{"truncated path", "1103a1a2", verified(AdvertUnknown, 0)},
		{"no payload after path", "1102a1a2", verified(AdvertUnknown, 0)},
		{"no payload after zero-hop path", "1100", verified(AdvertUnknown, 0)},
		// The 4 transport-code bytes are header, not payload.
		{"transport flood without payload", "101a2b3c4d00", verified(AdvertUnknown, 0)},
		{"transport direct share without payload", "130000000000", verified(AdvertUnknown, 0)},
		{"transport flood payload over MAX_PACKET_PAYLOAD", "101a2b3c4d00" + strings.Repeat("ab", 185), verified(AdvertUnknown, 0)},
		{"184-byte payload is valid", "1100" + strings.Repeat("ab", 184), verified(AdvertDirect, 0)},
		{"payload over MAX_PACKET_PAYLOAD", "1100" + strings.Repeat("ab", 185), verified(AdvertUnknown, 0)},
		{"truncated transport codes", "101a2b", verified(AdvertUnknown, 0)},
		{"not an advert (GRP_TXT)", "1500" + pl, verified(AdvertUnknown, 0)},
		{"bad hex header", "zz00" + pl, verified(AdvertUnknown, 0)},
		{"bad hex in payload", "1100aabbzz", verified(AdvertUnknown, 0)},
		{"odd length", "1100aab", verified(AdvertUnknown, 0)},
		{"one byte", "11", verified(AdvertUnknown, 0)},
		{"upper-case hex", "1100AABBCCDD", verified(AdvertDirect, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAdvertFrame(tc.raw); got != tc.want {
				t.Errorf("ClassifyAdvertFrame(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

// A missing path is unknown, an explicit empty path is an unverified direct
// hearing (a legacy row: a contact re-share cannot be excluded), and an
// observation's own frame wins over its path_json.
func TestClassifyAdvertPathJSONNullVsEmpty(t *testing.T) {
	for _, tc := range []struct {
		name, raw, path string
		want            AdvertHearing
	}{
		{"missing path, no frame", "", "", AdvertHearing{Kind: AdvertUnknown}},
		{"json null path", "", "null", AdvertHearing{Kind: AdvertUnknown}},
		{"malformed path", "", `["A1"`, AdvertHearing{Kind: AdvertUnknown}},
		{"explicit empty path", "", "[]", AdvertHearing{Kind: AdvertDirect}},
		{"explicit empty path with spaces", "", " [ ] ", AdvertHearing{Kind: AdvertDirect}},
		{"two hops", "", `["A1","B2"]`, AdvertHearing{Kind: AdvertRelayed, Hops: 2}},
		{"blank frame falls back to path", "  ", "[]", AdvertHearing{Kind: AdvertDirect}},
		{"frame shared beats '[]'", "130000000000aabbccdd", "[]", verified(AdvertShared, 0)},
		{"frame relayed beats '[]' (backfill sentinel)", "1102a1a2aabbccdd", "[]", verified(AdvertRelayed, 2)},
		{"undecodable frame does not fall back", "zz00aabb", "[]", verified(AdvertUnknown, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAdvert(tc.raw, tc.path); got != tc.want {
				t.Errorf("ClassifyAdvert(%q, %q) = %+v, want %+v", tc.raw, tc.path, got, tc.want)
			}
		})
	}
}

// The server classifies every advert observation it loads, so the frame
// classifier must not allocate.
func TestClassifyAdvertFrameDoesNotAllocate(t *testing.T) {
	raw := "101a2b3c4d02a1a2" + strings.Repeat("ab", 100)
	if n := testing.AllocsPerRun(1000, func() { ClassifyAdvertFrame(raw) }); n != 0 {
		t.Fatalf("ClassifyAdvertFrame allocates %.0f times per call, want 0", n)
	}
}

// BenchmarkClassifyAdvertFrame is the per-observation load cost the server
// pays (#488): a full classification for a 110-byte ADVERT frame, an early
// return on the header for any other payload type.
func BenchmarkClassifyAdvertFrame(b *testing.B) {
	for _, bc := range []struct{ name, raw string }{
		{"advert", "1103a1a2a3" + strings.Repeat("ab", 106)},
		{"non-advert", "1503a1a2a3" + strings.Repeat("ab", 106)},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				ClassifyAdvertFrame(bc.raw)
			}
		})
	}
}
