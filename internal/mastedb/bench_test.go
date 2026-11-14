package mastedb

import "testing"

// BenchmarkParse500 parses a full 500-row page, the most one request
// returns (#724 M1 perf proof).
func BenchmarkParse500(b *testing.B) {
	body := genGML(PageLimit, "500")
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Parse(body, testCell); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSites500 groups a full page into sites and derives the flags,
// including the UTM inverse projection of every site.
func BenchmarkSites500(b *testing.B) {
	page, err := Parse(genGML(PageLimit, "500"), testCell)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Sites(page.Rows)
	}
}

// BenchmarkSites5000 groups 5,000 rows, ten full pages' worth, into sites
// and derives the flags (#724 M2 plan: the cost of one server-side miss
// stays far below the upstream round trip even well past the page limit).
func BenchmarkSites5000(b *testing.B) {
	page, err := Parse(genGML(5000, "5000"), testCell)
	if err != nil {
		b.Fatal(err)
	}
	if len(page.Rows) != 5000 {
		b.Fatalf("parsed %d rows, want 5000", len(page.Rows))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Sites(page.Rows)
	}
}
