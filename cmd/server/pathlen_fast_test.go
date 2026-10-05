package main

import (
	"math/rand"
	"strings"
	"testing"
)

// pathLen is called once per observation of every candidate transmission in
// /api/nodes/{pk}/paths and /hop_analytics. The fast path must agree with the
// original json.Unmarshal-based implementation (pathLenSlow) for every input.

func TestPathLenFast_MatchesReference(t *testing.T) {
	cases := []string{
		"", " ", "invalid", "null", "true", "0", `""`, `"aa"`, `{}`, `{"a":1}`,
		`[]`, ` [] `, "\t[\n]\r\n", `[ ]`, `[,]`, `[""]`, `["",""]`,
		`["aa"]`, `["aa","bb"]`, `["aa","bb","cc"]`, ` [ "aa" , "bb" ] `,
		`["aa",]`, `[,"aa"]`, `["aa" "bb"]`, `["aa","bb"`, `["aa","bb"]]`,
		`["aa","bb"] x`, `x ["aa"]`, `["aa"]["bb"]`, `["aa"`, `["aa`, `[`, `]`,
		`[1,2,3]`, `[null]`, `[true,false]`, `["aa",1]`, `[["aa"],["bb"]]`,
		`[{"h":"aa"}]`, `["a\"b"]`, `["a\\b","c"]`, `["a,b","c]d","[e"]`,
		`["é"]`, `["日本"]`, "[\"a\x01b\"]", "[\"a\nb\"]", "[\"a\x7fb\"]",
		`["aa","bb","cc","dd","ee","ff","00","11"]`,
		`["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]`,
	}
	for _, in := range cases {
		if got, want := pathLen(in), pathLenSlow(in); got != want {
			t.Errorf("pathLen(%q) = %d, want %d (reference)", in, got, want)
		}
	}
}

// TestPathLenFast_RandomisedAgainstReference throws structurally "almost
// valid" JSON at both implementations. The token alphabet is chosen so most
// strings are close to the fast path's grammar, which is where a divergence
// would hide.
func TestPathLenFast_RandomisedAgainstReference(t *testing.T) {
	tokens := []string{
		"[", "]", ",", `"`, `"aa"`, `"bb"`, `""`, " ", "\n", "\t", `\`, `\"`,
		"a", "1", "null", "é", "\x00", "{", "}", ":", "-", "e",
	}
	rng := rand.New(rand.NewSource(1352))
	for i := 0; i < 300000; i++ {
		var b strings.Builder
		for j, n := 0, rng.Intn(14); j < n; j++ {
			b.WriteString(tokens[rng.Intn(len(tokens))])
		}
		in := b.String()
		if got, want := pathLen(in), pathLenSlow(in); got != want {
			t.Fatalf("pathLen(%q) = %d, want %d (reference)", in, got, want)
		}
	}
}

// TestPathLenFast_WellFormedPathsStayOnFastPath pins that the shapes real
// observations have (hex hop prefixes) are actually served by the
// allocation-free scanner, not the json.Unmarshal fallback.
func TestPathLenFast_WellFormedPathsStayOnFastPath(t *testing.T) {
	for _, in := range []string{`[]`, `["aa"]`, `["aa","bb","cc"]`, `["a1b2","c3d4"]`} {
		if _, ok := pathLenFast(in); !ok {
			t.Errorf("pathLenFast(%q) fell off the fast path", in)
		}
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = pathLen(`["aa","bb","cc"]`) }); allocs != 0 {
		t.Errorf("pathLen allocates %.0f times per call on a well-formed path, want 0", allocs)
	}
}

func BenchmarkPathLen(b *testing.B) {
	in := `["aa","bb","cc","dd","ee"]`
	b.Run("fast", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = pathLen(in)
		}
	})
	b.Run("reference_json_unmarshal", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = pathLenSlow(in)
		}
	})
}
