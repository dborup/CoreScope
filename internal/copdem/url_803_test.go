package copdem

import (
	"context"
	"net/http"
	"testing"

	"github.com/meshcore-analyzer/copdem/copdemtest"
	"github.com/meshcore-analyzer/dhm"
)

// #803: the live smoke check reads Tile.URL with a HeaderBytes range. Both
// must be what the client's first read of a tile sends.
func TestTileURLAndHeaderBytesAreTheClientsFirstRead(t *testing.T) {
	b := copdemtest.New()
	defer b.Close()
	b.Hook = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusInternalServerError)
		return true
	}
	c := NewClient(Options{Transport: b.Transport()})
	tile := Tile{Lat: 55, Lon: 12}
	if !tile.Listed() {
		t.Fatal("N55E012 not listed")
	}
	if _, err := c.header(context.Background(), tile); err == nil {
		t.Fatal("header read succeeded against a failing bucket")
	}
	log := b.Log()
	if len(log) != 1 {
		t.Fatalf("%d requests", len(log))
	}
	got := log[0].Scheme + "://" + log[0].Host + log[0].Path
	if log[0].RawQuery != "" {
		got += "?" + log[0].RawQuery
	}
	if got != tile.URL() {
		t.Fatalf("read %s, URL %s", got, tile.URL())
	}
	want := dhm.Range{From: 0, To: HeaderBytes - 1, MaxBody: HeaderBytes}.Header()
	if got := log[0].Header.Get("Range"); got != want {
		t.Fatalf("Range %q, want %q", got, want)
	}
}
