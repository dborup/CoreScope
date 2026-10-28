package main

import (
	"strings"
	"testing"
	"unsafe"
)

// #533: decodePersistedRelayPath reads the resolved_path column without encoding/json when it is in the compact form
// the ingestor writes (json.Marshal of []*string). Everything else goes
// through the JSON decoder, so the result must always equal what
// extractResolvedPubkeys(unmarshalResolvedPath(s)) gives.

const (
	pk533A = "a1" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab"
	pk533B = "B2" + "FEDCBA9876543210FEDCBA9876543210FEDCBA9876543210FEDCBA987654"
)

// resolved533Cases are column values on both sides of the compact grammar.
// Each one is checked against the JSON oracle, so a case only documents
// which side it is meant to exercise.
var resolved533Cases = []string{
	// compact form: scanned in place
	`[]`, `[null]`, `[null,null]`, `[""]`, `["",""]`,
	`["` + pk533A + `"]`,
	`["` + pk533A + `",null,"` + pk533B + `"]`,
	`["` + pk533A + `","` + pk533A + `"]`, // duplicates are kept
	`[null,"` + pk533B + `",""]`,
	`["ab","x y","{}","[1]","null"]`, // short, odd but valid strings, kept verbatim
	// anything else: JSON fallback
	``, `null`, `[`, `]`, `[,]`, `[null,]`, `[,null]`, `["a",]`, `["a"`, `["a"]]`,
	`["a"] `, ` ["a"]`, `[ "a"]`, `["a" ,"b"]`, `["a",` + "\n" + `"b"]`, "[\"a\"]\t",
	`[1]`, `[1,"` + pk533A + `"]`, `[true]`, `[{}]`, `[[]]`, `{}`, `"a"`, `nul`, `[nul]`, `[nulll]`, `[Null]`,
	`["a\"b"]`, `["a\\b"]`, `["A"]`, `["å"]`, `["\ud800"]`, `["a\/b"]`, `["\x"]`,
	"[\"a\tb\"]", "[\"a\x00b\"]", "[\"a\x1fb\"]", "[\"a\x7fb\"]",
	"[\"\xc3\xa5\"]", "[\"\xff\"]", "[\"\xed\xa0\x80\"]", // valid UTF-8, invalid byte, surrogate
	`["a"]["b"]`, `[null]x`, `[null]null`, `{"ab"]`, `xnull]`, ` "` + pk533A + `"]`,
}

func oracle533(s string) persistedRelayPath {
	if s == "" {
		return persistedRelayPath{}
	}
	return persistedRelayPath{pubkeys: extractResolvedPubkeys(unmarshalResolvedPath(s)), present: true}
}

func samePath533(a, b persistedRelayPath) bool {
	if a.present != b.present || len(a.pubkeys) != len(b.pubkeys) {
		return false
	}
	for i := range a.pubkeys {
		if a.pubkeys[i] != b.pubkeys[i] {
			return false
		}
	}
	return true
}

func TestDecodePersistedRelayPathMatchesJSON_533(t *testing.T) {
	for _, in := range resolved533Cases {
		if got, want := decodePersistedRelayPath(in), oracle533(in); !samePath533(got, want) {
			t.Errorf("decodePersistedRelayPath(%q) = %q/%v, JSON gives %q/%v", in, got.pubkeys, got.present, want.pubkeys, want.present)
		}
	}
}

// The scanner accepts exactly json.Marshal's output for []*string of plain
// printable-ASCII strings and rejects everything with an escape, whitespace,
// a non-string value or a byte outside 0x20..0x7e: those are what the JSON
// decoder rewrites (escapes, invalid UTF-8 -> U+FFFD) or rejects.
func TestScanResolvedPathPubkeysGrammar_533(t *testing.T) {
	accept := map[string][]string{
		`[]`:                                  {},
		`[null,""]`:                           {},
		`["` + pk533A + `",null,"ab"]`:        {pk533A, "ab"},
		`["` + pk533B + `","` + pk533B + `"]`: {pk533B, pk533B},
	}
	for in, want := range accept {
		got, ok := scanResolvedPathPubkeys(nil, in)
		if !ok || strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("scan(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	for _, in := range []string{``, `null`, `[ ]`, `["a\"b"]`, `["a\\b"]`, "[\"\xc3\xa5\"]", "[\"a\x7fb\"]", "[\"a\x1fb\"]",
		`[1]`, `[null,]`, `["a"] `, `[nulL]`, `["a"`, `[null]x`, `{"ab"]`, `xnull]`} {
		pre := []string{"keep"}
		if got, ok := scanResolvedPathPubkeys(pre, in); ok || len(got) != 1 || got[0] != "keep" {
			t.Errorf("scan(%q) = %q, %v; want the fallback (dst untouched, false)", in, got, ok)
		}
	}
}

// The pubkeys decodePersistedRelayPath returns are retained by the store as
// byNode/nodeHashes/byPathHop keys, so none may point into the column value:
// a 64-byte key would otherwise keep the whole row string alive.
func TestDecodePersistedRelayPathDoesNotPinInput_533(t *testing.T) {
	in := strings.Clone(`["` + pk533A + `",null,"` + pk533B + `"]`)
	lo := uintptr(unsafe.Pointer(unsafe.StringData(in)))
	hi := lo + uintptr(len(in))
	got := decodePersistedRelayPath(in)
	if len(got.pubkeys) != 2 {
		t.Fatalf("decoded %q", got.pubkeys)
	}
	for _, pk := range got.pubkeys {
		if p := uintptr(unsafe.Pointer(unsafe.StringData(pk))); p >= lo && p < hi {
			t.Errorf("pubkey %q points into the column value", pk)
		}
	}
}

// Allocation ceilings, the point of #533. The JSON decoder costs 7
// allocations for a 2-hop path (BenchmarkDecodePersistedRelayPath_533);
// the scan costs the result slice and one copy per kept hop.
func TestPersistedRelayPathAllocs_533(t *testing.T) {
	in := `["` + pk533A + `",null,"` + pk533B + `"]`
	if a := testing.AllocsPerRun(200, func() { decodePersistedRelayPath(in) }); a > 3 {
		t.Errorf("decodePersistedRelayPath: %.0f allocs, want <= 3 (slice + one copy per hop)", a)
	}
}

func FuzzPersistedRelayPath_533(f *testing.F) {
	for _, c := range resolved533Cases {
		f.Add(c)
	}
	f.Fuzz(func(t *testing.T, in string) {
		want := oracle533(in)
		if got := decodePersistedRelayPath(in); !samePath533(got, want) {
			t.Fatalf("decodePersistedRelayPath(%q) = %q/%v, JSON gives %q/%v", in, got.pubkeys, got.present, want.pubkeys, want.present)
		}
	})
}
