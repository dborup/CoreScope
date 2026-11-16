package profilecard

import (
	"fmt"
	"math"

	"github.com/meshcore-analyzer/cardkit"
)

// endpointLabel is what the card calls endpoint i (0 = A): a map point by
// its coordinates (it has no name, whatever the caller passes), a node by
// its drawable name, or "Node A"/"Node B" when names are off or the name
// cannot be drawn.
func endpointLabel(fs *cardkit.Fonts, end TerrainEndpoint, name string, i int, hide bool) string {
	tag := "AB"[i : i+1]
	if end.Kind == "point" {
		if !finite(end.Lat) || !finite(end.Lon) {
			return "Point " + tag
		}
		return fmt.Sprintf("%.4f, %.4f", end.Lat, end.Lon)
	}
	if !hide {
		if n := fs.DrawableName(name); n != "" {
			return n
		}
	}
	return "Node " + tag
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
