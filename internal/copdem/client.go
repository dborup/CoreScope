package copdem

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// Host is the one upstream host: the copernicus-dem-30m bucket of AWS
// Open Data (eu-central-1), read anonymously over HTTPS. It is fixed in
// code; no configuration can change it.
const Host = "copernicus-dem-30m.s3.amazonaws.com"

const baseURL = "https://" + Host

const (
	// HeaderBytes is the first read of a tile: GLO-30 keeps every IFD and
	// block table in its first ~1.3 KB (IFDS_BEFORE_DATA). The live smoke
	// check (#803) reads the same range.
	HeaderBytes = 16 << 10
	// maxHeaderBytes is a header after its one extension.
	maxHeaderBytes = 64 << 10
	// maxBlockBody caps a block read: the largest block the header parser
	// admits (dhm.MaxTileSize² float32 plus its slack) and some margin.
	// The largest real GLO-30 block measured is 2.78 MB.
	maxBlockBody = dhm.MaxTileSize*dhm.MaxTileSize*4 + 64<<10

	maxHeaderEntries = 64 // tiles; the region has 60
	headerMeta       = 9  // per cached header: object size, cold calls
	maxBlockEntries  = 4096
)

// Options are the client's limits.
type Options struct {
	// Timeout bounds one upstream request, from sending it to the end of
	// its body. Waiting for a concurrency slot is not counted; the
	// caller's context bounds that.
	Timeout time.Duration
	// Concurrency is the number of upstream requests in flight, and of
	// blocks being decoded, at once. It is the fill's own budget: DHM's
	// four slots are never used.
	Concurrency int
	// MaxPending bounds distinct upstream calls, queued or running
	// (dhm.Admission); beyond it a call fails at once. It is a backstop:
	// the profile's own admission (maxInFlight) bounds the fills, so the
	// server sets it to maxInFlight × dhm.MaxCallsPerModel and every
	// admitted profile can run its whole fill.
	MaxPending int
	// BlockCacheBytes bounds the compressed blocks kept in memory.
	BlockCacheBytes int
	// CacheTTL is how long a header or block is reused. GLO-30 on AWS
	// has not changed since May 2022.
	CacheTTL time.Duration
	// Transport replaces the HTTP transport (tests: copdemtest). nil is a
	// clone of http.DefaultTransport without transparent compression.
	Transport http.RoundTripper
	Now       func() time.Time
}

// DefaultOptions are the server defaults.
func DefaultOptions() Options {
	return Options{
		Timeout:         10 * time.Second,
		Concurrency:     2,
		MaxPending:      4 * dhm.MaxCallsPerModel, // the profile's default maxInFlight
		BlockCacheBytes: 32 << 20,
		CacheTTL:        24 * time.Hour,
		Now:             time.Now,
	}
}

// Client reads GLO-30 tiles by range. It holds no credential of any kind:
// there is nothing to send, and the DHM token lives only in dhm.Client.
type Client struct {
	opts     Options
	http     *http.Client
	sem      chan struct{} // upstream requests in flight
	decode   chan struct{} // blocks being decoded
	adm      *dhm.Admission
	flight   dhm.Flight[[]byte]
	headers  *dhm.Cache[Tile]     // size, cold calls, then the header bytes
	blocks   *dhm.Cache[blockKey] // stored block bytes
	requests atomic.Int64         // upstream requests sent
	// afterMiss, set only by tests before any call, runs between a
	// caller's cache miss and its shared read.
	afterMiss func()
}

// blockKey is one block of one level of one tile.
type blockKey struct {
	tile         Tile
	level, block int
}

// NewClient returns a client on opts; a zero limit takes the default.
func NewClient(opts Options) *Client {
	def := DefaultOptions()
	if opts.Timeout <= 0 {
		opts.Timeout = def.Timeout
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = def.Concurrency
	}
	if opts.MaxPending < 1 {
		opts.MaxPending = def.MaxPending
	}
	if opts.BlockCacheBytes < 1 {
		opts.BlockCacheBytes = def.BlockCacheBytes
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = def.CacheTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	tr := opts.Transport
	if tr == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		// Byte ranges of a GeoTIFF: transparent gzip could only change
		// what Content-Range describes.
		t.DisableCompression = true
		tr = t
	}
	// Never follow a redirect, to any host: a 3xx is not a 206 and fails
	// like any other status. Each request's context carries its timeout.
	hc := dhm.NewNoRedirectClient(0)
	hc.Transport = tr
	return &Client{
		opts:    opts,
		http:    hc,
		sem:     make(chan struct{}, opts.Concurrency),
		decode:  make(chan struct{}, opts.Concurrency),
		adm:     dhm.NewAdmission(opts.MaxPending),
		headers: dhm.NewCache[Tile](maxHeaderEntries, maxHeaderEntries*(maxHeaderBytes+headerMeta), opts.CacheTTL),
		blocks:  dhm.NewCache[blockKey](maxBlockEntries, opts.BlockCacheBytes, opts.CacheTTL),
	}
}

// Requests is the number of upstream requests the client has sent.
func (c *Client) Requests() int64 { return c.requests.Load() }

// header is a tile's parsed layout.
type header struct {
	tiff *dhm.TiledGeoTIFF
	size int64 // the object's length
	// calls is what reading the header cold costs, 1 or 2 requests,
	// whether or not this read was cached: the fill's budget counts it
	// so, and the choice of level never depends on the cache.
	calls int
}

// header reads t's layout: one range read of HeaderBytes, extended once
// up to maxHeaderBytes when the IFDs continue. Only a listed tile is read
// (rangeGet).
func (c *Client) header(ctx context.Context, t Tile) (header, error) {
	enc, err := cachedRead(c, ctx, c.headers, t, "h "+t.Path(), func(work context.Context) ([]byte, error) {
		return c.fetchHeader(work, t)
	})
	if err != nil {
		return header{}, err
	}
	size := int64(binary.LittleEndian.Uint64(enc))
	tiff, err := dhm.ParseTiledGeoTIFF(enc[headerMeta:], size)
	if err != nil {
		return header{}, dhm.FetchError{Reason: "invalid header"}
	}
	return header{tiff: tiff, size: size, calls: int(enc[8])}, nil
}

// fetchHeader runs the cold read of t's header and returns it encoded
// for the cache, after checking that it parses and that it places the
// raster on t (#795): a valid COG of another tile is never cached, so
// never sampled.
func (c *Client) fetchHeader(work context.Context, t Tile) ([]byte, error) {
	path := t.Path()
	buf, size, err := c.rangeGet(work, path, 0, HeaderBytes-1, 0, HeaderBytes)
	if err != nil {
		return nil, err
	}
	calls := 1
	tiff, err := dhm.ParseTiledGeoTIFF(buf, size)
	if errors.Is(err, dhm.ErrShortHeader) && len(buf) == HeaderBytes {
		more, _, err2 := c.rangeGet(work, path, HeaderBytes, min(maxHeaderBytes, size)-1, size, maxHeaderBytes-HeaderBytes)
		if err2 != nil {
			return nil, err2
		}
		buf, calls = append(buf, more...), 2
		tiff, err = dhm.ParseTiledGeoTIFF(buf, size)
	}
	switch {
	case errors.Is(err, dhm.ErrShortHeader):
		return nil, dhm.FetchError{Reason: "header too large"}
	case err != nil:
		return nil, dhm.FetchError{Reason: "invalid header"}
	case !tiff.Fits(float64(t.Lon), float64(t.Lat), float64(t.Lon+1), float64(t.Lat+1)):
		return nil, errGeoref
	}
	enc := make([]byte, headerMeta, headerMeta+len(buf))
	binary.LittleEndian.PutUint64(enc, uint64(size))
	enc[8] = byte(calls)
	return append(enc, buf...), nil
}

// errGeoref: the tile's raster is not on its 1° square.
var errGeoref = dhm.FetchError{Reason: "wrong georeference"}

// block reads the stored bytes of block i of level of t.
func (c *Client) block(ctx context.Context, t Tile, h header, level, i int) ([]byte, error) {
	if level < 0 || level >= len(h.tiff.Levels) || i < 0 || i >= h.tiff.Levels[level].NumBlocks() {
		return nil, dhm.ErrBadRequest
	}
	path := t.Path()
	off, n := h.tiff.Levels[level].BlockRange(i)
	return cachedRead(c, ctx, c.blocks, blockKey{t, level, i}, fmt.Sprintf("b %s %d-%d", path, off, off+n-1), func(work context.Context) ([]byte, error) {
		b, _, err := c.rangeGet(work, path, off, off+n-1, h.size, maxBlockBody)
		return b, err
	})
}

// cachedRead returns key's bytes from cache, or reads them once through
// the flight under flightKey, shared by every caller asking meanwhile.
// The read looks in the cache again and stores what it read before its
// call ends (#834): a caller that missed the cache just as an equal read
// ended starts a new call, and that call finds the bytes, so every caller
// either joins the read or finds its result. A failed read stores nothing.
func cachedRead[K comparable](c *Client, ctx context.Context, cache *dhm.Cache[K], key K, flightKey string, read func(context.Context) ([]byte, error)) ([]byte, error) {
	now := c.opts.Now()
	if raw, ok := cache.Get(key, now); ok {
		return raw, nil
	}
	if c.afterMiss != nil {
		c.afterMiss()
	}
	raw, _, err := c.flight.DoAdmitted(ctx, flightKey, 0, c.adm, func(work context.Context) ([]byte, error) {
		if raw, ok := cache.Get(key, now); ok {
			return raw, nil
		}
		raw, err := read(work)
		if err != nil {
			return nil, err
		}
		cache.Put(key, raw, now)
		return raw, nil
	})
	if err != nil {
		return nil, dhm.ContextFailure(err)
	}
	return raw, nil
}

// rangeGet reads bytes [from, to] of path through dhm.RangeRead: a 206
// whose Content-Range starts at from and ends at to, or at the object's
// last byte when it is shorter; size, when known (> 0), must be the
// object's length; and the body must be exactly that range, at most
// maxBody bytes. It returns the body and the object's length. Every
// failure is a dhm.FetchError with a fixed reason, or the allow-list's
// refusal: this is the one place a request is made, and checkPath comes
// first.
func (c *Client) rangeGet(work context.Context, path string, from, to, size int64, maxBody int) ([]byte, int64, error) {
	if err := checkPath(path); err != nil {
		return nil, 0, err
	}
	r := dhm.Range{From: from, To: to, Size: size, MaxBody: maxBody}
	if !r.Valid() {
		return nil, 0, dhm.ErrBadRequest
	}
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-work.Done():
		return nil, 0, dhm.FetchError{Reason: "canceled"}
	}
	ctx, cancel := context.WithTimeout(work, c.opts.Timeout)
	defer cancel()
	return dhm.RangeRead(ctx, c.http, pathURL(path), r, &c.requests)
}
