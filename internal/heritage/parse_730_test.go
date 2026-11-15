package heritage

import (
	"strings"
	"testing"
)

// TestParseVertexBudgetIsPerDocument (#730 M1a): MaxVertices bounds the
// whole page, not one feature, so a response cannot make the Nearest work
// unbounded by spreading its vertices over many features. The budget moved
// into dhm.PolygonReader with the geometry reading; this pins that one
// reader still covers every feature of the page.
func TestParseVertexBudgetIsPerDocument(t *testing.T) {
	ring := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = `[526000,6179000]`
		}
		return `[[` + strings.Join(parts, ",") + `]]`
	}
	// Two features of MaxVertices/2 + 1 vertices each: either alone fits,
	// together they do not.
	half := MaxVertices/2 + 1
	if _, err := Parse(fc(featureJSON("Polygon", ring(half), ""), "")); err != nil {
		t.Fatalf("one feature under the cap refused: %v", err)
	}
	body := fc(featureJSON("Polygon", ring(half), "")+","+featureJSON("Polygon", ring(half), ""), "")
	if _, err := Parse(body); err == nil {
		t.Fatalf("%d vertices over two features accepted (cap %d)", 2*half, MaxVertices)
	}
}
