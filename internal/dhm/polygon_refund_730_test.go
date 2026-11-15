package dhm

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestReadRefundsAFailedRead (#730 M1b, from the #771 review): a Read that
// fails charges nothing, so a caller that skips a bad feature and reads on
// (internal/buildings) keeps the whole budget for the good ones. The
// refused polygon below has a valid five-vertex outer ring and then a
// three-position hole: the outer ring was already validated when the hole
// failed.
func TestReadRefundsAFailedRead(t *testing.T) {
	const badHole = `[[526040,6179040],[526060,6179040],[526040,6179040]]`
	r := reader(10)
	if _, err := r.Read("Polygon", json.RawMessage(`[`+square+`,`+badHole+`]`)); !errors.Is(err, ErrGeometry) {
		t.Fatalf("bad hole: err %v, want ErrGeometry", err)
	}
	if r.Vertices() != 0 {
		t.Fatalf("a failed Read charged %d vertices, want 0", r.Vertices())
	}
	// The second polygon of a MultiPolygon fails: the first is refunded too.
	if _, err := r.Read("MultiPolygon", json.RawMessage(`[[`+square+`],[`+badHole+`]]`)); err == nil {
		t.Fatal("bad second polygon accepted")
	}
	if r.Vertices() != 0 {
		t.Fatalf("a failed MultiPolygon charged %d vertices, want 0", r.Vertices())
	}
	// The budget is whole: two good squares (10 vertices) still fit.
	if _, err := r.Read("MultiPolygon", json.RawMessage(`[[`+square+`],[`+square+`]]`)); err != nil {
		t.Fatalf("10 vertices after refunds refused: %v", err)
	}
	if r.Vertices() != 10 {
		t.Fatalf("vertices %d, want 10", r.Vertices())
	}
}
