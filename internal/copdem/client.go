package copdem

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
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
	// headerBytes is the first read of a tile: GLO-30 keeps every IFD and
	// block table in its first ~1.3 KB (IFDS_BEFORE_DATA).
	headerBytes = 16 << 10
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
	return &Client{
		opts: opts,
		http: &http.Client{
			Transport: tr,
			// Never follow a redirect, to any host: a 3xx is not a 206 and
			// fails like any other status.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
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

// header reads t's layout: one range read of headerBytes, extended once
// up to maxHeaderBytes when the IFDs continue. Only a listed tile is read
// (rangeGet).
func (c *Client) header(ctx context.Context, t Tile) (header, error) {
	path := t.Path()
	now := c.opts.Now()
	enc, ok := c.headers.Get(t, now)
	if !ok {
		var err error
		enc, _, err = c.flight.DoAdmitted(ctx, "h "+path, 0, c.adm, func(work context.Context) ([]byte, error) {
			return c.fetchHeader(work, path)
		})
		if err != nil {
			return header{}, dhm.ContextFailure(err)
		}
		c.headers.Put(t, enc, now)
	}
	size := int64(binary.LittleEndian.Uint64(enc))
	tiff, err := dhm.ParseTiledGeoTIFF(enc[headerMeta:], size)
	if err != nil {
		return header{}, dhm.FetchError{Reason: "invalid header"}
	}
	return header{tiff: tiff, size: size, calls: int(enc[8])}, nil
}

// fetchHeader runs the cold read of a header and returns it encoded for
// the cache, after checking that it parses.
func (c *Client) fetchHeader(work context.Context, path string) ([]byte, error) {
	buf, size, err := c.rangeGet(work, path, 0, headerBytes-1, 0, headerBytes)
	if err != nil {
		return nil, err
	}
	calls := 1
	_, err = dhm.ParseTiledGeoTIFF(buf, size)
	if errors.Is(err, dhm.ErrShortHeader) && len(buf) == headerBytes {
		more, _, err2 := c.rangeGet(work, path, headerBytes, min(maxHeaderBytes, size)-1, size, maxHeaderBytes-headerBytes)
		if err2 != nil {
			return nil, err2
		}
		buf, calls = append(buf, more...), 2
		_, err = dhm.ParseTiledGeoTIFF(buf, size)
	}
	switch {
	case errors.Is(err, dhm.ErrShortHeader):
		return nil, dhm.FetchError{Reason: "header too large"}
	case err != nil:
		return nil, dhm.FetchError{Reason: "invalid header"}
	}
	enc := make([]byte, headerMeta, headerMeta+len(buf))
	binary.LittleEndian.PutUint64(enc, uint64(size))
	enc[8] = byte(calls)
	return append(enc, buf...), nil
}

// block reads the stored bytes of block i of level of t.
func (c *Client) block(ctx context.Context, t Tile, h header, level, i int) ([]byte, error) {
	if level < 0 || level >= len(h.tiff.Levels) || i < 0 || i >= h.tiff.Levels[level].NumBlocks() {
		return nil, dhm.ErrBadRequest
	}
	key := blockKey{t, level, i}
	now := c.opts.Now()
	if raw, ok := c.blocks.Get(key, now); ok {
		return raw, nil
	}
	path := t.Path()
	off, n := h.tiff.Levels[level].BlockRange(i)
	raw, _, err := c.flight.DoAdmitted(ctx, fmt.Sprintf("b %s %d-%d", path, off, off+n-1), 0, c.adm, func(work context.Context) ([]byte, error) {
		b, _, err := c.rangeGet(work, path, off, off+n-1, h.size, maxBlockBody)
		return b, err
	})
	if err != nil {
		return nil, dhm.ContextFailure(err)
	}
	c.blocks.Put(key, raw, now)
	return raw, nil
}

// contentRangeRe is the one Content-Range shape accepted: a single
// satisfied range of a known length.
var contentRangeRe = regexp.MustCompile(`^bytes ([0-9]{1,15})-([0-9]{1,15})/([0-9]{1,15})$`)

// rangeGet reads bytes [from, to] of path. The answer must be a 206 whose
// Content-Range starts at from and ends at to, or at the object's last
// byte when it is shorter; size, when known (> 0), must be the object's
// length; and the body must be exactly that range, at most maxBody bytes.
// It returns the body and the object's length. Every failure is a
// dhm.FetchError with a fixed reason, or the allow-list's refusal: this
// is the one place a request is made, and checkPath comes first.
func (c *Client) rangeGet(work context.Context, path string, from, to, size int64, maxBody int) ([]byte, int64, error) {
	if err := checkPath(path); err != nil {
		return nil, 0, err
	}
	if from < 0 || to < from || to-from+1 > int64(maxBody) {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, 0, dhm.FetchError{Reason: "request"}
	}
	req.Header.Set("Range", "bytes="+strconv.FormatInt(from, 10)+"-"+strconv.FormatInt(to, 10))
	c.requests.Add(1)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, transportFailure(err)
	}
	// Closed unread on every refusal: a body that is not the range asked
	// for is never downloaded.
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, 0, dhm.FetchError{Reason: "status " + strconv.Itoa(resp.StatusCode)}
	}
	m := contentRangeRe.FindStringSubmatch(resp.Header.Get("Content-Range"))
	if m == nil {
		return nil, 0, dhm.FetchError{Reason: "bad content range"}
	}
	a, _ := strconv.ParseInt(m[1], 10, 64)
	b, _ := strconv.ParseInt(m[2], 10, 64)
	total, _ := strconv.ParseInt(m[3], 10, 64)
	if a != from || b < a || b >= total || (b != to && !(b == total-1 && to > b)) || (size > 0 && total != size) {
		return nil, 0, dhm.FetchError{Reason: "bad content range"}
	}
	n := b - a + 1
	// One byte more than the range shows a longer body; nothing past it
	// is read, whatever Content-Length says.
	body, err := io.ReadAll(io.LimitReader(resp.Body, n+1))
	if err != nil {
		return nil, 0, transportFailure(err)
	}
	if int64(len(body)) != n {
		return nil, 0, dhm.FetchError{Reason: "bad content length"}
	}
	return body, total, nil
}

// transportFailure reduces a transport or body error to its category.
func transportFailure(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || dhm.IsTimeout(err) {
		return dhm.FetchError{Reason: "timeout"}
	}
	if errors.Is(err, context.Canceled) {
		return dhm.FetchError{Reason: "canceled"}
	}
	return dhm.FetchError{Reason: "transport"}
}
