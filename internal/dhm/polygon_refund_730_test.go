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

// TestReadBudgetErrorIsDistinct: overrunning the budget is ErrVertexBudget,
// which is still an ErrGeometry for callers that refuse the whole document
// (internal/heritage), while a caller that skips bad features can stop at
// the budget instead of skipping every feature after it. The overrun is
// refunded like any other failure, so Vertices stays the accepted count.
func TestReadBudgetErrorIsDistinct(t *testing.T) {
	r := reader(8)
	if _, err := r.Read("Polygon", json.RawMessage(`[`+square+`]`)); err != nil {
		t.Fatal(err)
	}
	_, err := r.Read("Polygon", json.RawMessage(`[`+square+`]`))
	if !errors.Is(err, ErrVertexBudget) || !errors.Is(err, ErrGeometry) {
		t.Fatalf("overrun: err %v, want ErrVertexBudget wrapping ErrGeometry", err)
	}
	if r.Vertices() != 5 {
		t.Fatalf("vertices %d after the overrun, want 5", r.Vertices())
	}
	// A shape error is not a budget error.
	_, err = reader(100).Read("Point", json.RawMessage(`[526000,6179000]`))
	if !errors.Is(err, ErrGeometry) || errors.Is(err, ErrVertexBudget) {
		t.Fatalf("shape error %v reads as a budget error", err)
	}
}
