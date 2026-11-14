package profilecard

import "testing"

// BenchmarkRender is one complete card — draw plus PNG encode — for a
// realistic model=both profile with 1024 returned samples. The budget it
// is held to is TestRenderWithinBudget's 25 ms.
func BenchmarkRender(b *testing.B) {
	resp := fxMarginal()
	pal := DefaultPalette(true)
	names := [2]string{"Ridge North", "Valley Node"}
	b.ReportAllocs()
	var size int
	for i := 0; i < b.N; i++ {
		out, err := Render(resp, names, pal, Options{})
		if err != nil {
			b.Fatal(err)
		}
		size = len(out)
	}
	b.ReportMetric(float64(size), "bytes/card")
}

// BenchmarkDraw is the drawing alone, without the PNG encode.
func BenchmarkDraw(b *testing.B) {
	resp := fxMarginal()
	pal := DefaultPalette(true)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := render(resp, [2]string{"Ridge North", "Valley Node"}, pal, Options{}); err != nil {
			b.Fatal(err)
		}
	}
}
