package copdem

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcore-analyzer/copdem/copdemtest"
	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/dhm/dhmtest"
)

// #738 M1 — the Copernicus range client. Every test runs against
// copdemtest's fake bucket through an injected transport; nothing reaches
// the network, and no token exists here.

var cph = Tile{55, 12} // Copenhagen and Malmö

// smallTile is a small COG for t: 40 × 24 pixels in 16-pixel blocks, so
// the client tests stay fast; the fill tests use the real GLO-30 layout.
func smallTile(t Tile) []byte {
	return dhmtest.COG(dhmtest.COGOptions{
		Width: 40, Height: 24, Tile: 16,
		Lon0: float64(t.Lon), Lat0: float64(t.Lat + 1), PixelLon: 1.0 / 40, PixelLat: 1.0 / 24,
		Value:    func(lon, lat float64) float32 { return float32(lon + lat) },
		Overview: true,
	})
}

func newTestClient(t *testing.T, b *copdemtest.Bucket, edit ...func(*Options)) *Client {
	t.Helper()
	o := DefaultOptions()
	o.Transport = b.Transport()
	o.Timeout = 2 * time.Second
	for _, e := range edit {
		e(&o)
	}
	return NewClient(o)
}

func newBucket(t *testing.T) *copdemtest.Bucket {
	b := copdemtest.New()
	t.Cleanup(b.Close)
	return b
}

func failure(err error) string { return dhm.FailureReason(err) }

func TestClientReadsHeaderAndBlock(t *testing.T) {
	b := newBucket(t)
	file := smallTile(cph)
	b.Put(cph.Path(), file)
	c := newTestClient(t, b)
	ctx := context.Background()

	h, err := c.header(ctx, cph)
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	if h.size != int64(len(file)) || h.calls != 1 || len(h.tiff.Levels) != 2 || h.tiff.Levels[0].NumBlocks() != 6 {
		t.Fatalf("header %+v", h)
	}
	raw, err := c.block(ctx, cph, h, 0, 4)
	off, n := h.tiff.Levels[0].BlockRange(4)
	if err != nil || string(raw) != string(file[off:off+n]) {
		t.Fatalf("block 4: %v (%d bytes, want %d)", err, len(raw), n)
	}
	if got := b.Requests.Load(); got != 2 {
		t.Fatalf("%d upstream requests, want 2", got)
	}
	// Both are cached now.
	if _, err := c.header(ctx, cph); err != nil {
		t.Fatal(err)
	}
	if _, err := c.block(ctx, cph, h, 0, 4); err != nil {
		t.Fatal(err)
	}
	if got := b.Requests.Load(); got != 2 {
		t.Fatalf("%d upstream requests after cache hits, want 2", got)
	}
	// The first read is exactly the 16 KiB header range, the block read
	// exactly the block.
	log := b.Log()
	if r := log[0].Header.Get("Range"); r != "bytes=0-16383" {
		t.Fatalf("header range %q", r)
	}
	if r := log[1].Header.Get("Range"); r != fmt.Sprintf("bytes=%d-%d", off, off+n-1) {
		t.Fatalf("block range %q", r)
	}
}

// Every request is a plain GET of an allow-listed tile path on the one
// host, with a Range and nothing that could carry a credential: no token
// header (DHM's), no Authorization, no cookie, no query.
func TestClientRequestShape(t *testing.T) {
	b := newBucket(t)
	b.Put(cph.Path(), smallTile(cph))
	b.Put((Tile{55, 13}).Path(), smallTile(Tile{55, 13}))
	c := newTestClient(t, b)
	for _, tl := range []Tile{cph, {55, 13}} {
		h, err := c.header(context.Background(), tl)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < h.tiff.Levels[0].NumBlocks(); i++ {
			if _, err := c.block(context.Background(), tl, h, 0, i); err != nil {
				t.Fatal(err)
			}
		}
	}
	log := b.Log()
	if len(log) != 2+12 {
		t.Fatalf("%d requests", len(log))
	}
	for _, r := range log {
		if r.Method != http.MethodGet || r.Scheme != "https" || r.Host != Host || r.RawQuery != "" || checkPath(r.Path) != nil {
			t.Fatalf("request %s %s://%s%s?%s", r.Method, r.Scheme, r.Host, r.Path, r.RawQuery)
		}
		for k := range r.Header {
			switch strings.ToLower(k) {
			case "range", "user-agent":
			default:
				t.Fatalf("request header %q sent", k)
			}
		}
		if !strings.HasPrefix(r.Header.Get("Range"), "bytes=") {
			t.Fatalf("Range %q", r.Header.Get("Range"))
		}
	}
}

// A tile that is not listed (sea, or outside the region) is never
// requested; a listed tile the bucket does not have is an error, never 0.
func TestClientTileListGate(t *testing.T) {
	b := newBucket(t)
	c := newTestClient(t, b)
	for _, tl := range []Tile{{55, 16}, {52, 12}, {55, 30}} {
		if _, err := c.header(context.Background(), tl); err == nil {
			t.Errorf("%v: header read", tl)
		}
	}
	if b.Requests.Load() != 0 {
		t.Fatalf("%d requests for unlisted tiles", b.Requests.Load())
	}
	_, err := c.header(context.Background(), cph)
	if failure(err) != "status 404" {
		t.Fatalf("listed tile missing upstream: %v, want status 404", err)
	}
}

// A server that ignores Range answers 200 with the whole object: refused
// without reading it, so a 15–33 MB tile can never be pulled in.
func TestClientRefusesIgnoredRange(t *testing.T) {
	b := newBucket(t)
	b.Put(cph.Path(), smallTile(cph))
	var served atomic.Int64
	b.Hook = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Length", "33000000")
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 64<<10)
		for i := 0; i < 33000000/len(chunk); i++ {
			n, err := w.Write(chunk)
			served.Add(int64(n))
			if err != nil {
				break
			}
		}
		return true
	}
	c := newTestClient(t, b)
	_, err := c.header(context.Background(), cph)
	if failure(err) != "status 200" {
		t.Fatalf("200 to a range request: %v", err)
	}
	if c.headers.Len() != 0 {
		t.Fatal("a refused header was cached")
	}
}

// Only a 206 whose Content-Range is exactly the range asked for, of the
// object size the header read found, and whose body is exactly that long.
func TestClientChecksContentRange(t *testing.T) {
	file := smallTile(cph)
	size := int64(len(file))
	for name, answer := range map[string]func(w http.ResponseWriter, from, to int64){
		"no Content-Range": func(w http.ResponseWriter, from, to int64) {
			w.WriteHeader(http.StatusPartialContent)
			w.Write(file[from : to+1])
		},
		"other start": func(w http.ResponseWriter, from, to int64) {
			copdemtest.WriteRange(w, file[from+1:to+1], from+1, size)
		},
		"shorter": func(w http.ResponseWriter, from, to int64) {
			copdemtest.WriteRange(w, file[from:to], from, size)
		},
		"longer": func(w http.ResponseWriter, from, to int64) {
			copdemtest.WriteRange(w, file[from:to+2], from, size)
		},
		"other size": func(w http.ResponseWriter, from, to int64) {
			copdemtest.WriteRange(w, file[from:to+1], from, size+1)
		},
		"malformed": func(w http.ResponseWriter, from, to int64) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", from, to))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(file[from : to+1])
		},
		"body shorter than Content-Range": func(w http.ResponseWriter, from, to int64) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, size))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(file[from:to])
		},
		"body longer than Content-Range": func(w http.ResponseWriter, from, to int64) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, size))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(file[from : to+2])
		},
		"multipart": func(w http.ResponseWriter, from, to int64) {
			w.Header().Set("Content-Type", "multipart/byteranges; boundary=x")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(file[from : to+1])
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBucket(t)
			b.Put(cph.Path(), file)
			c := newTestClient(t, b)
			h, err := c.header(context.Background(), cph) // honest
			if err != nil {
				t.Fatal(err)
			}
			off, n := h.tiff.Levels[0].BlockRange(2)
			b.Hook = func(w http.ResponseWriter, r *http.Request) bool {
				answer(w, off, off+n-1)
				return true
			}
			if _, err := c.block(context.Background(), cph, h, 0, 2); err == nil {
				t.Fatal("accepted")
			}
			if c.blocks.Len() != 0 {
				t.Fatal("a refused block was cached")
			}
		})
	}
}

// Nothing above the caps is ever asked for: a block larger than its
// level's MaxBlockBytes cannot pass the header parser, and a range over
// the client's own body cap is refused before any request.
func TestClientBodyCaps(t *testing.T) {
	b := newBucket(t)
	c := newTestClient(t, b)
	if _, _, err := c.rangeGet(context.Background(), cph.Path(), 0, maxBlockBody, 0, maxBlockBody); err == nil {
		t.Fatal("a range one byte over the cap was requested")
	}
	if b.Requests.Load() != 0 {
		t.Fatal("over-cap range reached upstream")
	}
	if maxBlockBody < dhm.MaxTileSize*dhm.MaxTileSize*4+8192 || maxBlockBody > 5<<20 {
		t.Fatalf("maxBlockBody %d: must hold any block the parser admits, and stay near 4 MiB", maxBlockBody)
	}
}

// A redirect is never followed, to any host.
func TestClientNeverFollowsRedirect(t *testing.T) {
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	for _, loc := range []string{other.URL + cph.Path(), "https://" + Host + cph.Path()} {
		b := newBucket(t)
		b.Put(cph.Path(), smallTile(cph))
		b.Hook = func(w http.ResponseWriter, r *http.Request) bool {
			http.Redirect(w, r, loc, http.StatusFound)
			return true
		}
		c := newTestClient(t, b)
		if _, err := c.header(context.Background(), cph); failure(err) != "status 302" {
			t.Fatalf("redirect to %s: %v", loc, err)
		}
		if b.Requests.Load() != 1 {
			t.Fatalf("redirect to %s: %d bucket requests", loc, b.Requests.Load())
		}
	}
	if elsewhere.Load() != 0 {
		t.Fatal("a redirect was followed to another host")
	}
}

// A header whose IFDs continue past the first 16 KiB is read once more,
// up to 64 KiB; one that needs more is an error.
func TestClientExtendsHeaderOnce(t *testing.T) {
	file := longHeaderTile()
	b := newBucket(t)
	b.Put(cph.Path(), file)
	c := newTestClient(t, b)
	h, err := c.header(context.Background(), cph)
	if err != nil {
		t.Fatalf("extended header: %v", err)
	}
	if h.calls != 2 || b.Requests.Load() != 2 || h.tiff.Levels[0].NumBlocks() != 4096 {
		t.Fatalf("calls %d, requests %d, blocks %d", h.calls, b.Requests.Load(), h.tiff.Levels[0].NumBlocks())
	}
	if r := b.Log()[1].Header.Get("Range"); r != fmt.Sprintf("bytes=16384-%d", maxHeaderBytes-1) {
		t.Fatalf("extension range %q", r)
	}
	// Cached with its cold cost: still 2 calls on a hit, and no request.
	if h2, err := c.header(context.Background(), cph); err != nil || h2.calls != 2 || b.Requests.Load() != 2 {
		t.Fatalf("cached: %+v %v", h2, err)
	}

	// The GeoKeyDirectory pointed past 64 KiB: still short after one
	// extension.
	o2 := smallCOG(cph)
	far := uint32(100 << 10)
	o2.Pad = 200 << 10
	o2.Edit = func(level int, es []dhmtest.COGEntry) []dhmtest.COGEntry {
		for i := range es {
			if es[i].Tag == 34735 {
				es[i].Raw = &far
			}
		}
		return es
	}
	b2 := newBucket(t)
	b2.Put(cph.Path(), dhmtest.COG(o2))
	if _, err := newTestClient(t, b2).header(context.Background(), cph); failure(err) != "header too large" {
		t.Fatalf("header past 64 KiB: %v", err)
	}
	if b2.Requests.Load() != 2 {
		t.Fatalf("%d requests, want 2 (one extension, no more)", b2.Requests.Load())
	}
}

// longHeaderTile is 1024² in 16-pixel blocks: 4096 blocks, 32 KiB of
// block tables, so its header needs the extension read.
func longHeaderTile() []byte {
	return dhmtest.COG(dhmtest.COGOptions{Width: 1024, Height: 1024, Tile: 16, Lon0: 12, Lat0: 56, PixelLon: 1.0 / 1024, PixelLat: 1.0 / 1024,
		Value: func(lon, lat float64) float32 { return 7 }, Uncompressed: true})
}

// The extension read is tied to the object the first read saw: a second
// range that reports another object length is refused, never stitched
// onto the first (#773 review finding 4b).
func TestClientHeaderExtensionKeepsObjectSize(t *testing.T) {
	file := longHeaderTile()
	b := newBucket(t)
	b.Put(cph.Path(), file)
	var n atomic.Int64
	b.Hook = func(w http.ResponseWriter, r *http.Request) bool {
		if n.Add(1) != 2 {
			return false
		}
		var from, to int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			t.Errorf("Range %q: %v", r.Header.Get("Range"), err)
		}
		// The right bytes, but of an object one byte longer.
		copdemtest.WriteRange(w, file[from:to+1], from, int64(len(file))+1)
		return true
	}
	c := newTestClient(t, b)
	if _, err := c.header(context.Background(), cph); failure(err) != "bad content range" {
		t.Fatalf("extension of another object: %v", err)
	}
	if n.Load() != 2 || c.headers.Len() != 0 {
		t.Fatalf("%d requests, %d cached headers", n.Load(), c.headers.Len())
	}
}

func smallCOG(t Tile) dhmtest.COGOptions {
	return dhmtest.COGOptions{
		Width: 40, Height: 24, Tile: 16,
		Lon0: float64(t.Lon), Lat0: float64(t.Lat + 1), PixelLon: 1.0 / 40, PixelLat: 1.0 / 24,
		Value: func(lon, lat float64) float32 { return 1 },
	}
}

// A file that is not a GLO-30-shaped GeoTIFF is an error, and not cached.
func TestClientRefusesInvalidHeader(t *testing.T) {
	b := newBucket(t)
	b.Put(cph.Path(), append([]byte("II*\x00\x08\x00\x00\x00"), make([]byte, 100)...))
	c := newTestClient(t, b)
	if _, err := c.header(context.Background(), cph); failure(err) != "invalid header" {
		t.Fatalf("garbage header: %v", err)
	}
	if c.headers.Len() != 0 {
		t.Fatal("cached")
	}
}

// Concurrent reads of one block share one upstream request (dhm.Flight).
func TestClientCoalescesBlocks(t *testing.T) {
	b := newBucket(t)
	b.Put(cph.Path(), smallTile(cph))
	c := newTestClient(t, b)
	h, err := c.header(context.Background(), cph)
	if err != nil {
		t.Fatal(err)
	}
	release, open := gate(t)
	b.Hook = func(w http.ResponseWriter, r *http.Request) bool {
		<-release
		return false
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.block(context.Background(), cph, h, 0, 3)
			errs <- err
		}()
	}
	waitFor(t, func() bool { return c.flight.Pending() == 8 })
	open()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := b.Requests.Load(); got != 2 { // header + one block
		t.Fatalf("%d requests, want 2", got)
	}
}

// gate is a channel that hooks block on until the test opens it. It is
// also opened on cleanup, before the bucket closes, so a failing test
// never leaves a handler blocked (httptest.Server.Close waits for them).
func gate(t *testing.T) (<-chan struct{}, func()) {
	ch := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(open)
	return ch, open
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// rangeGet classifies its own transport failures through
// dhm.TransportFailure with the request's context (#773 review finding
// 5): a caller that leaves mid-request is "canceled", the per-request
// timeout is "timeout". The flight above it reports the caller's own
// context, so only a direct read shows this.
func TestClientRangeGetTransportFailure(t *testing.T) {
	b := newBucket(t)
	b.Put(cph.Path(), smallTile(cph))
	b.Hook = func(w http.ResponseWriter, r *http.Request) bool {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		return true
	}
	c := newTestClient(t, b, func(o *Options) { o.Timeout = 3 * time.Second })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	if _, _, err := c.rangeGet(ctx, cph.Path(), 0, 15, 0, 16); failure(err) != "canceled" {
		t.Fatalf("cancelled mid-request: %v", err)
	}
	c = newTestClient(t, b, func(o *Options) { o.Timeout = 30 * time.Millisecond })
	if _, _, err := c.rangeGet(context.Background(), cph.Path(), 0, 15, 0, 16); failure(err) != "timeout" {
		t.Fatalf("per-request timeout: %v", err)
	}
}

// A cancelled read stops its upstream request and frees its slot; a
// read waiting for a slot can be cancelled without reaching upstream; a
// caller's deadline is "timeout" (mirrors internal/dhm client_ctx_test).
func TestClientCancelAndTimeoutFreeTheSlot(t *testing.T) {
	b := newBucket(t)
	b.Put(cph.Path(), smallTile(cph))
	gone := make(chan struct{})
	var first atomic.Bool
	b.Hook = func(w http.ResponseWriter, r *http.Request) bool {
		if first.CompareAndSwap(false, true) {
			select {
			case <-r.Context().Done():
				close(gone)
			case <-time.After(5 * time.Second):
			}
			return true
		}
		return false
	}
	c := newTestClient(t, b, func(o *Options) { o.Concurrency = 1; o.Timeout = 3 * time.Second })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	if _, err := c.header(ctx, cph); failure(err) != "canceled" {
		t.Fatalf("cancelled: %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("cancel took %v", el)
	}
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request kept running after cancel")
	}
	start = time.Now()
	next, cancelNext := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelNext()
	if _, err := c.header(next, cph); err != nil {
		t.Fatalf("next read (slot not released?): %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("slot not released: %v", el)
	}

	// Waiting for a slot: cancelled before reaching upstream.
	before := b.Requests.Load()
	select {
	case c.sem <- struct{}{}:
	default:
		t.Fatal("the slot is still taken")
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel2)
	h, _ := c.header(context.Background(), cph) // cached
	if _, err := c.block(ctx2, cph, h, 0, 0); failure(err) != "canceled" {
		t.Fatalf("cancelled while queued: %v", err)
	}
	<-c.sem
	if b.Requests.Load() != before {
		t.Fatal("a request queued for a slot reached upstream after cancel")
	}

	// The caller's deadline.
	b.Hook = func(w http.ResponseWriter, r *http.Request) bool {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		return true
	}
	ctx3, cancel3 := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel3()
	if _, err := c.block(ctx3, cph, h, 0, 1); failure(err) != "timeout" {
		t.Fatalf("deadline: %v", err)
	}
	// The client's own per-request timeout, on a request that hangs.
	c2 := newTestClient(t, b, func(o *Options) { o.Timeout = 100 * time.Millisecond })
	start = time.Now()
	if _, err := c2.header(context.Background(), cph); failure(err) != "timeout" || time.Since(start) > time.Second {
		t.Fatalf("per-request timeout: %v after %v", err, time.Since(start))
	}
}

// The admission bound: with every slot taken by running calls, a new
// distinct call is refused at once, with no request (dhm.Admission).
func TestClientAdmission(t *testing.T) {
	b := newBucket(t)
	b.Put(cph.Path(), smallTile(cph))
	c := newTestClient(t, b, func(o *Options) { o.MaxPending = 1 })
	h, err := c.header(context.Background(), cph)
	if err != nil {
		t.Fatal(err)
	}
	release, open := gate(t)
	b.Hook = func(w http.ResponseWriter, r *http.Request) bool { <-release; return false }
	done := make(chan error, 1)
	go func() { _, err := c.block(context.Background(), cph, h, 0, 0); done <- err }()
	waitFor(t, func() bool { return c.adm.InUse() == 1 })
	before := b.Requests.Load()
	// Bounded, so a missing bound fails here instead of waiting forever.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.block(ctx, cph, h, 0, 1); !errors.Is(err, dhm.ErrAdmissionFull) {
		t.Fatalf("second call: %v, want admission full", err)
	}
	if b.Requests.Load() != before {
		t.Fatal("a refused call reached upstream")
	}
	open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The block cache is bounded by bytes: an LRU that never holds more than
// BlockCacheBytes.
func TestClientBlockCacheIsBounded(t *testing.T) {
	b := newBucket(t)
	file := smallTile(cph)
	b.Put(cph.Path(), file)
	c := newTestClient(t, b)
	h, err := c.header(context.Background(), cph)
	if err != nil {
		t.Fatal(err)
	}
	_, n0 := h.tiff.Levels[0].BlockRange(0)
	_, n1 := h.tiff.Levels[0].BlockRange(1)
	c = newTestClient(t, b, func(o *Options) { o.BlockCacheBytes = int(max(n0, n1)) + 1 })
	h, _ = c.header(context.Background(), cph)
	for i := 0; i < 6; i++ {
		if _, err := c.block(context.Background(), cph, h, 0, i); err != nil {
			t.Fatal(err)
		}
		if c.blocks.Size() > int(max(n0, n1))+1 {
			t.Fatalf("block cache holds %d bytes", c.blocks.Size())
		}
	}
	if c.blocks.Len() >= 6 {
		t.Fatalf("%d blocks cached: nothing evicted", c.blocks.Len())
	}
	if c.headers.Len() != 1 || c.headers.Size() > maxHeaderEntries*(maxHeaderBytes+headerMeta) {
		t.Fatalf("header cache %d entries, %d bytes", c.headers.Len(), c.headers.Size())
	}
}

// The production transport never asks for gzip and never follows a
// redirect; nothing outside tests can point the client elsewhere.
func TestNewClientDefaults(t *testing.T) {
	c := NewClient(DefaultOptions())
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok || !tr.DisableCompression {
		t.Fatalf("transport %T: compression must be disabled", c.http.Transport)
	}
	if c.http.CheckRedirect == nil || c.http.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirects would be followed")
	}
	if baseURL != "https://copernicus-dem-30m.s3.amazonaws.com" || Host != copdemtest.Host {
		t.Fatalf("base URL %q", baseURL)
	}
	o := DefaultOptions()
	if o.Concurrency != 2 || o.MaxPending != 4*dhm.MaxCallsPerModel || o.BlockCacheBytes != 32<<20 || o.Timeout != 10*time.Second {
		t.Fatalf("defaults %+v", o)
	}
}
