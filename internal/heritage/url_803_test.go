package heritage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// #803: the live smoke check requests CellURL. It must be exactly the URL
// FetchCell sends.
func TestCellURLIsWhatFetchCellSends(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RequestURI()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL+"/wfs")
	x := Cell{E: 2898, N: 24703}
	if !x.Valid() {
		t.Fatal("test Cell not valid")
	}
	if _, err := c.FetchCell(context.Background(), x); err == nil {
		t.Fatal("fetch succeeded against a failing upstream")
	}
	if want := c.CellURL(x); srv.URL+got != want {
		t.Fatalf("sent %s\nwant %s", srv.URL+got, want)
	}
}
