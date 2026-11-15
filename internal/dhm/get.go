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
// (internal/mastedb, internal/heritage): timeout per request including the
// body, and no redirect is ever followed, so a request only reaches the
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, FetchError{"request"}
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, transportFailure(ctx, err)
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
		return nil, transportFailure(ctx, err)
	}
	if len(data) > maxBody {
		return nil, FetchError{"oversize response"}
	}
	return data, nil
}

// transportFailure maps a request or body read error to a category.
func transportFailure(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return FetchError{"canceled"}
	case errors.Is(err, context.DeadlineExceeded) || IsTimeout(err):
		return FetchError{"timeout"}
	}
	return FetchError{"transport"}
}
