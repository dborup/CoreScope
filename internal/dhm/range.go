package dhm

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"
)

// Range is one byte range of a remote object for RangeRead: bytes
// [From, To], at most MaxBody of them. Size, when known (> 0), is the
// object's length, and the answer must agree with it.
type Range struct {
	From, To, Size int64
	MaxBody        int
}

// Valid reports whether r can be asked for: a non-empty range from a
// non-negative offset that fits MaxBody.
func (r Range) Valid() bool {
	return r.From >= 0 && r.To >= r.From && r.To-r.From+1 <= int64(r.MaxBody)
}

// Header is the Range header value RangeRead sends for r.
func (r Range) Header() string {
	return "bytes=" + strconv.FormatInt(r.From, 10) + "-" + strconv.FormatInt(r.To, 10)
}

// contentRangeRe is the one Content-Range shape accepted: a single
// satisfied range of a known length.
var contentRangeRe = regexp.MustCompile(`^bytes ([0-9]{1,15})-([0-9]{1,15})/([0-9]{1,15})$`)

// RangeRead reads r of target with one GET through hc, which must not
// follow redirects (NewNoRedirectClient). The answer must be a 206 whose
// Content-Range starts at r.From and ends at r.To, or at the object's last
// byte when it is shorter; whose length is r.Size when that is known; and
// whose body is exactly that range. Nothing past it is read. It returns
// the body and the object's length. sent, if not nil, counts the requests
// sent.
//
// Every failure is ErrBadRequest (an invalid r: nothing is sent) or a
// FetchError with a fixed reason, never a URL or upstream text. The
// shared range read of internal/copdem (#738) and internal/skraafoto
// (#737); each checks its own host and path before calling it.
func RangeRead(ctx context.Context, hc *http.Client, target string, r Range, sent *atomic.Int64) ([]byte, int64, error) {
	if !r.Valid() {
		return nil, 0, ErrBadRequest
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, FetchError{Reason: "request"}
	}
	req.Header.Set("Range", r.Header())
	if sent != nil {
		sent.Add(1)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, TransportFailure(ctx, err)
	}
	// Closed unread on every refusal: a body that is not the range asked
	// for is never downloaded.
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, 0, FetchError{Reason: "status " + strconv.Itoa(resp.StatusCode)}
	}
	m := contentRangeRe.FindStringSubmatch(resp.Header.Get("Content-Range"))
	if m == nil {
		return nil, 0, FetchError{Reason: "bad content range"}
	}
	a, _ := strconv.ParseInt(m[1], 10, 64)
	b, _ := strconv.ParseInt(m[2], 10, 64)
	total, _ := strconv.ParseInt(m[3], 10, 64)
	if a != r.From || b < a || b >= total || (b != r.To && !(b == total-1 && r.To > b)) || (r.Size > 0 && total != r.Size) {
		return nil, 0, FetchError{Reason: "bad content range"}
	}
	n := b - a + 1
	// One byte more than the range shows a longer body; nothing past it
	// is read, whatever Content-Length says.
	body, err := io.ReadAll(io.LimitReader(resp.Body, n+1))
	if err != nil {
		return nil, 0, TransportFailure(ctx, err)
	}
	if int64(len(body)) != n {
		return nil, 0, FetchError{Reason: "bad content length"}
	}
	return body, total, nil
}
