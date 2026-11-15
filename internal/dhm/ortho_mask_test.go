package dhm

import (
	"math"
	"testing"
)

// #728 M3 — the mask of ortho tiles DHM does not cover (ortho_mask.go).

// tileAt is the zoom-z tile holding p.
func tileAt(p LatLon, z int) TileKey {
	n := math.Exp2(float64(z))
	return TileKey{Z: z, X: lonToTileX(p.Lon, n), Y: latToTileY(p.Lat, n)}
}

// The window is every z12 tile that meets Coverage, and nothing else, so a
// client that tests a tile outside it falls back to `bounds`.
func TestOrthoForeignMaskWindow(t *testing.T) {
	m := OrthoForeignMask()
	if m.Z != OrthoMaskZoom || OrthoMaskZoom != 12 {
		t.Fatalf("mask zoom %d, OrthoMaskZoom %d", m.Z, OrthoMaskZoom)
	}
	if len(m.Bits) != (m.Cols*m.Rows+7)/8 {
		t.Fatalf("%d bytes for %dx%d tiles", len(m.Bits), m.Cols, m.Rows)
	}
	for r := -1; r <= m.Rows; r++ {
		for c := -1; c <= m.Cols; c++ {
			k := TileKey{Z: m.Z, X: m.X0 + c, Y: m.Y0 + r}
			inside := r >= 0 && c >= 0 && r < m.Rows && c < m.Cols
			if k.IntersectsCoverage() != inside {
				t.Fatalf("tile %v: intersects coverage %v, in the window %v", k, k.IntersectsCoverage(), inside)
			}
		}
	}
}

// The safety property: no point of a marked tile is Covered, so the
// client never skips a tile that could show Denmark. Each marked tile is
// probed on a 17×17 grid, edges and corners included; the same probe
// finds Covered points in the tiles left unmarked, so it can tell.
func TestOrthoForeignMaskNeverHidesCoverage(t *testing.T) {
	m := OrthoForeignMask()
	const steps = 16
	marked, unmarkedCovered := 0, 0
	for r := 0; r < m.Rows; r++ {
		for c := 0; c < m.Cols; c++ {
			k := TileKey{Z: m.Z, X: m.X0 + c, Y: m.Y0 + r}
			bb := k.BBox()
			covered := false
			for i := 0; i <= steps && !covered; i++ {
				for j := 0; j <= steps; j++ {
					lon, lat := mercatorToLonLat(bb[0]+float64(i)/steps*(bb[2]-bb[0]), bb[1]+float64(j)/steps*(bb[3]-bb[1]))
					if Covers(ToUTM32(lat, lon)) {
						covered = true
						break
					}
				}
			}
			if m.Has(k) {
				marked++
				if covered {
					t.Fatalf("tile %v is marked but holds a covered point", k)
				}
			} else if covered {
				unmarkedCovered++
			}
		}
	}
	if marked == 0 || unmarkedCovered == 0 {
		t.Fatalf("marked %d, unmarked with coverage %d: the probe tells nothing", marked, unmarkedCovered)
	}
	t.Logf("%d of %d z12 tiles marked", marked, m.Cols*m.Rows)
}

func TestOrthoForeignMaskPlaces(t *testing.T) {
	m := OrthoForeignMask()
	for _, c := range []struct {
		name string
		p    LatLon
		want bool
	}{
		// Danish land and sea, and coasts with foreign land in the tile:
		// never marked.
		{"Copenhagen", copenhagen, false},
		{"Padborg", LatLon{54.8256, 9.3640}, false},
		{"Tønder", tonder, false},
		{"Store Okseø", storeOkseo, false},
		{"Christiansø", LatLon{55.3205, 15.1872}, false},
		{"Rønne", ronne, false},
		{"Gedser", LatLon{54.5747, 11.9269}, false},
		{"Saltholm", saltholm, false},
		{"Kattegat mid", LatLon{56.5000, 11.8000}, false},
		{"Øresund mid-strait", LatLon{55.7500, 12.8000}, false},
		{"Malmö (Øresund in the tile)", malmo, false},
		{"Helsingborg (Øresund in the tile)", helsingborg, false},
		{"Flensburg (Flensborg Fjord in the tile)", flensburg, false},
		// The plan's outside-DK checks (#728 M3): Skåne and Schleswig
		// away from the coast.
		{"Hörby, Skåne", LatLon{55.8500, 13.6600}, true},
		{"Lund, Skåne", LatLon{55.7000, 13.1900}, true},
		{"Kristianstad, Skåne", LatLon{56.0300, 14.1600}, true},
		{"Ystad, Skåne", ystad, true},
		{"Tarp, Schleswig", LatLon{54.6650, 9.4000}, true},
		{"Schleswig (south of the raster extent)", LatLon{54.5150, 9.5600}, true},
	} {
		for _, z := range []int{12, 15, 20} {
			if got := m.Has(tileAt(c.p, z)); got != c.want {
				t.Errorf("%s at z%d: marked %v, want %v", c.name, z, got, c.want)
			}
		}
	}
}

func TestTileMaskHas(t *testing.T) {
	// 3×2 window at z12 from (10, 20); marked: (11, 20) and (10, 21).
	m := TileMask{Z: 12, X0: 10, Y0: 20, Cols: 3, Rows: 2, Bits: []byte{1<<1 | 1<<3}}
	for _, c := range []struct {
		k    TileKey
		want bool
	}{
		{TileKey{12, 11, 20}, true},
		{TileKey{12, 10, 21}, true},
		{TileKey{12, 10, 20}, false},
		{TileKey{12, 12, 21}, false},
		// Children of a marked tile are marked, at any depth.
		{TileKey{13, 22, 40}, true},
		{TileKey{13, 23, 41}, true},
		{TileKey{20, 11<<8 + 255, 20 << 8}, true},
		{TileKey{13, 24, 40}, false},
		// Outside the window, or coarser than the mask: unmarked.
		{TileKey{12, 9, 20}, false},
		{TileKey{12, 13, 20}, false},
		{TileKey{12, 11, 19}, false},
		{TileKey{12, 11, 22}, false},
		{TileKey{11, 5, 10}, false},
	} {
		if got := m.Has(c.k); got != c.want {
			t.Errorf("Has(%v) = %v, want %v", c.k, got, c.want)
		}
	}
}

// The mask ships in every /api/config/client of an ortho instance.
func TestOrthoForeignMaskSize(t *testing.T) {
	m := OrthoForeignMask()
	if len(m.Bits) > 1024 {
		t.Fatalf("mask is %d bytes; the client config should stay small", len(m.Bits))
	}
}

// Built once per process, when the ortho proxy starts.
func BenchmarkOrthoForeignMask(b *testing.B) {
	fl := foreignLand()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		foreignTileMask(fl, OrthoMaskZoom)
	}
}
