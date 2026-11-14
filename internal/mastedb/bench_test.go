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
