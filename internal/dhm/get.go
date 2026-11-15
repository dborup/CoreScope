package dhm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

// NewNoRedirectClient is the HTTP client of a keyless upstream
// (internal/mastedb, internal/heritage, internal/copdem): timeout per
// request including the body (0: none, the request's context bounds it),
// and no redirect is ever followed, so a request only reaches the
// endpoint whose shape was checked.
func NewNoRedirectClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// GetBody runs one GET and returns the body of a 200 response whose media
// type is one of contentTypes, at most maxBody bytes long. accept, if not
// empty, is sent as the Accept header; no other header is set. Every
// failure is a FetchError with a fixed category ("canceled", "timeout",
// "transport", "status 500", "unexpected content type", "oversize
// response", ...): never a URL, never upstream text.
func GetBody(ctx context.Context, hc *http.Client, target, accept string, contentTypes []string, maxBody int) ([]byte, error) {
	h := http.Header{}
	if accept != "" {
		h.Set("Accept", accept)
	}
	return GetBodyWithHeader(ctx, hc, target, h, contentTypes, maxBody)
}

// GetBodyWithHeader is GetBody sending exactly the headers in header: the
// Skråfoto STAC client (#737) sends its token header through it. Its
// errors are GetBody's, so they never hold a header value either.
func GetBodyWithHeader(ctx context.Context, hc *http.Client, target string, header http.Header, contentTypes []string, maxBody int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, FetchError{"request"}
	}
	for k, vs := range header {
		req.Header[k] = append([]string(nil), vs...)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, TransportFailure(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, int64(maxBody)))
		return nil, FetchError{"status " + strconv.Itoa(resp.StatusCode)}
	}
	if !HasContentType(resp.Header.Get("Content-Type"), contentTypes) {
		return nil, FetchError{"unexpected content type"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBody)+1))
	if err != nil {
		return nil, TransportFailure(ctx, err)
	}
	if len(data) > maxBody {
		return nil, FetchError{"oversize response"}
	}
	return data, nil
}

// TransportFailure maps a request or body read error to its category:
// "canceled" when ctx was cancelled, "timeout" for a deadline or a
// transport timeout, else "transport". ctx is the request's context.
// GetBody and internal/copdem's range reads share it.
func TransportFailure(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return FetchError{"canceled"}
	case errors.Is(err, context.DeadlineExceeded) || IsTimeout(err):
		return FetchError{"timeout"}
	}
	return FetchError{"transport"}
}
