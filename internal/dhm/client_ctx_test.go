package dhm

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// #683 review N3 — a context runs through FetchRaster and Client: a
// cancelled request, or the profile deadline, stops the upstream call and
// frees its concurrency slot instead of holding it until Timeout.

func TestClientCancelStopsUpstreamAndReleasesSlot(t *testing.T) {
	var first atomic.Bool
	upstreamGone := make(chan struct{})
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if first.CompareAndSwap(false, true) {
			select {
			case <-r.Context().Done(): // the client hung up
				close(upstreamGone)
			case <-time.After(5 * time.Second):
			}
			return
		}
		got, _ := parseCoverageQuery(r.URL.Query())
		w.Header().Set("Content-Type", "image/tiff")
		w.Write(rasterTIFF(got, binary.LittleEndian))
	})
	c := newTestClient(t, u, &logSink{})
	c.opts.Timeout = 3 * time.Second
	c.sem = make(chan struct{}, 1) // one slot
	src := NewWCSSource(c)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := src.FetchRaster(ctx, someRaster())
	var fe FetchError
	if !errors.As(err, &fe) || fe.Reason != "canceled" {
		t.Fatalf("cancelled fetch: %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("cancel took %v", el)
	}
	select {
	case <-upstreamGone:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request kept running after cancel")
	}
	// The slot is free again: the next raster does not wait for Timeout.
	start = time.Now()
	r := someRaster()
	r.BBox[0] += 100
	if _, err := src.FetchRaster(context.Background(), r); err != nil {
		t.Fatalf("next fetch: %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("slot not released: next fetch took %v", el)
	}
}

func TestClientCancelWhileWaitingForSlot(t *testing.T) {
	u := tiffUpstream(t)
	c := newTestClient(t, u, &logSink{})
	c.sem = make(chan struct{}, 1)
	c.sem <- struct{}{} // budget full
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	_, err := NewWCSSource(c).FetchRaster(ctx, someRaster())
	var fe FetchError
	if !errors.As(err, &fe) || fe.Reason != "canceled" {
		t.Fatalf("err %v, want canceled (not busy)", err)
	}
	if u.hits.Load() != 0 {
		t.Fatal("cancelled request reached upstream")
	}
}

func TestClientCallerDeadlineIsTimeout(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	c := newTestClient(t, u, &logSink{})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := NewWCSSource(c).FetchRaster(ctx, someRaster())
	var fe FetchError
	if !errors.As(err, &fe) || fe.Reason != "timeout" {
		t.Fatalf("err %v, want timeout", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("caller deadline ignored: %v (client Timeout 2s)", el)
	}
}

// The #680 tile path keeps its own lifetime: Get takes no context.
func TestTileGetStillWorks(t *testing.T) {
	u := newUpstream(t, nil)
	if _, err := newTestClient(t, u, &logSink{}).FetchHillshadeTile(someTile()); err != nil {
		t.Fatal(err)
	}
}

// #686 follow-up — the dhm_DAF allow-list check is the terrain hillshade
// check: it accepts exactly the terrain tile query and nothing the
// exaggerated layer would send.
func TestWMSCheckIsTerrainHillshadeCheck(t *testing.T) {
	check := allowedServices[WMSService].check
	for z := MinZoom; z <= MaxZoom; z++ {
		k := lonLatTile(z, 10.2, 56.16)
		if err := check(k.hillshadeQuery(HillshadeTerrain)); err != nil {
			t.Fatalf("z%d: terrain query rejected: %v", z, err)
		}
		if err := check(k.hillshadeQuery(HillshadeExaggerated)); err == nil {
			t.Fatalf("z%d: exaggerated layer accepted on dhm_DAF", z)
		}
		q := k.hillshadeQuery(HillshadeTerrain)
		q.Set("transparent", "true")
		if err := check(q); err == nil {
			t.Fatalf("z%d: browser spelling accepted upstream", z)
		}
	}
	if err := check(url.Values{}); err == nil {
		t.Fatal("empty query accepted")
	}
}
