// Package copdemtest is a fake copernicus-dem-30m bucket for the #738
// foreign-fill tests in internal/copdem and cmd/server. It answers range
// reads the way S3 does, records every request, and is reached through a
// RoundTripper, so the client under test still addresses the real host:
// nothing here, and nothing using it, ever reaches the network.
package copdemtest

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
)

// Host is the bucket's host, the only one the Transport serves.
const Host = "copernicus-dem-30m.s3.amazonaws.com"

// Request is one request the bucket saw.
type Request struct {
	Method, Scheme, Host, Path, RawQuery string
	Header                               http.Header
}

// Bucket is the fake bucket.
type Bucket struct {
	srv   *httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	log   []Request
	// Hook, when set, answers a request instead of the bucket when it
	// returns true: errors, redirects, a server that ignores Range.
	Hook func(w http.ResponseWriter, r *http.Request) bool
	// Generate, when set, makes the object at a path that was not Put
	// (nil: 404), once; tests serve a whole region without building it.
	Generate func(path string) []byte
	// Requests counts requests that reached the bucket.
	Requests atomic.Int64
	// Bytes counts body bytes the bucket served.
	Bytes atomic.Int64
}

// New starts a bucket; Close stops it.
func New() *Bucket {
	b := &Bucket{files: map[string][]byte{}}
	b.srv = httptest.NewServer(http.HandlerFunc(b.serve))
	return b
}

func (b *Bucket) Close() { b.srv.Close() }

// URL is the bucket's own address, for tests that need a second host.
func (b *Bucket) URL() string { return b.srv.URL }

// Put stores data at path ("/Copernicus_DSM_COG_10_.../...tif").
func (b *Bucket) Put(path string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.files[path] = data
}

// Log is a copy of every request so far.
func (b *Bucket) Log() []Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Request(nil), b.log...)
}

var errHost = errors.New("copdemtest: request for a host other than the bucket")

// Transport sends requests for https://Host to the bucket. A request for
// any other scheme or host fails, and is logged, so a test sees it.
func (b *Bucket) Transport() http.RoundTripper {
	target, _ := url.Parse(b.srv.URL)
	inner := b.srv.Client().Transport
	return roundTrip(func(r *http.Request) (*http.Response, error) {
		b.mu.Lock()
		b.log = append(b.log, Request{Method: r.Method, Scheme: r.URL.Scheme, Host: r.URL.Host,
			Path: r.URL.Path, RawQuery: r.URL.RawQuery, Header: r.Header.Clone()})
		b.mu.Unlock()
		if r.URL.Scheme != "https" || r.URL.Host != Host {
			return nil, errHost
		}
		out := r.Clone(r.Context())
		out.URL.Scheme, out.URL.Host, out.Host = target.Scheme, target.Host, ""
		return inner.RoundTrip(out)
	})
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// object is the file at path, generated on first use.
func (b *Bucket) object(path string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if data, ok := b.files[path]; ok {
		return data, true
	}
	if b.Generate == nil {
		return nil, false
	}
	data := b.Generate(path)
	if data != nil {
		b.files[path] = data
	}
	return data, data != nil
}

var rangeRe = regexp.MustCompile(`^bytes=([0-9]+)-([0-9]+)$`)

func (b *Bucket) serve(w http.ResponseWriter, r *http.Request) {
	b.Requests.Add(1)
	if b.Hook != nil && b.Hook(w, r) {
		return
	}
	data, ok := b.object(r.URL.Path)
	if !ok {
		http.Error(w, "NoSuchKey", http.StatusNotFound)
		return
	}
	m := rangeRe.FindStringSubmatch(r.Header.Get("Range"))
	if m == nil {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		n, _ := w.Write(data)
		b.Bytes.Add(int64(n))
		return
	}
	from, _ := strconv.ParseInt(m[1], 10, 64)
	to, _ := strconv.ParseInt(m[2], 10, 64)
	size := int64(len(data))
	if from >= size || to < from {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	to = min(to, size-1)
	WriteRange(w, data[from:to+1], from, size)
	b.Bytes.Add(to - from + 1)
}

// WriteRange answers 206 with part, the bytes at from of a size-byte
// object, as S3 does; hooks use it to answer with a different range.
func WriteRange(w http.ResponseWriter, part []byte, from, size int64) {
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, from+int64(len(part))-1, size))
	w.Header().Set("Content-Length", strconv.Itoa(len(part)))
	w.Header().Set("Content-Type", "image/tiff")
	w.WriteHeader(http.StatusPartialContent)
	w.Write(part)
}
