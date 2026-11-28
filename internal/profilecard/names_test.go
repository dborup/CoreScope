package profilecard

import (
	"testing"

	"github.com/meshcore-analyzer/cardkit"
)

func TestEndpointLabel(t *testing.T) {
	fonts := mustFonts(t)
	node := TerrainEndpoint{Kind: "node", Lat: 55.123456, Lon: 10.654321}
	point := TerrainEndpoint{Kind: "point", Lat: 55.123456, Lon: -3.5}
	cases := []struct {
		end  TerrainEndpoint
		name string
		i    int
		hide bool
		want string
	}{
		{node, "Søby Bakker", 0, false, "Søby Bakker"},
		{node, "Søby Bakker", 1, true, "Node B"}, // names off: never the name
		{node, "东京", 0, false, "Node A"},
		{node, "", 1, false, "Node B"},
		// A map point has no name, whatever the caller passes.
		{point, "Ignored", 0, false, "55.1235, -3.5000"},
		{point, "", 1, true, "55.1235, -3.5000"},
		// Kind missing or odd: treated as a node, never as a name leak.
		{TerrainEndpoint{}, "", 0, false, "Node A"},
	}
	for _, c := range cases {
		if got := endpointLabel(fonts, c.end, c.name, c.i, c.hide); got != c.want {
			t.Errorf("endpointLabel(%+v, %q, %d, %v) = %q, want %q", c.end, c.name, c.i, c.hide, got, c.want)
		}
	}
}

func mustFonts(t testing.TB) *cardkit.Fonts {
	t.Helper()
	f, err := cardkit.LoadFonts()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// #745: a surface above the antenna at an endpoint gets the panel's own
// chip, shown whatever the terrain says, instead of "Blocked".
func TestSurfaceChipAntennaBelow(t *testing.T) {
	base := fxClear()
	cases := []struct {
		endpoint, reason string
		show             bool
		want             string
	}{
		{"A", "antennaBelowSurface", true, "Buildings/trees: antenna below the surface at A — check the antenna height"},
		{"both", "antennaBelowSurface", true, "Buildings/trees: antenna below the surface at A and B — check the antenna height"},
		{"B", "antennaBelowSurface", true, "Buildings/trees: antenna below the surface at B — check the antenna height"},
		{"C", "antennaBelowSurface", true, "Buildings/trees: Blocked"}, // unknown endpoint: the plain chip
		{"A", "", true, "Buildings/trees: Blocked"},
	}
	for _, c := range cases {
		resp := base
		resp.Verdict.Surface = &TerrainSurfaceInfo{State: "blocked", Reason: c.reason, Endpoint: c.endpoint}
		got, show := surfaceChip(resp)
		if show != c.show || got.label != c.want || got.tone != "warn" {
			t.Errorf("%s/%q: chip %+v show=%v, want %q", c.endpoint, c.reason, got, show, c.want)
		}
		_, runs, err := render(resp, [2]string{"Roof Node", "Hill"}, DefaultPalette(true), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got := runText(runs, "surface-badge"); got != c.want {
			t.Errorf("%s: drawn chip %q, want %q", c.endpoint, got, c.want)
		}
		checkCard(t, "antenna below "+c.endpoint, encodePNG(t, mustRender(t, resp, [2]string{"Roof Node", "Hill"})), runs)
	}
	// A partial profile shows no surface chip at all (#689 N1).
	resp := fxPartial()
	resp.Verdict.Surface = &TerrainSurfaceInfo{State: "blocked", Reason: "antennaBelowSurface", Endpoint: "A"}
	if _, show := surfaceChip(resp); show {
		t.Error("surface chip shown on a partial profile")
	}
}
