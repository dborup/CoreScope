package dhm

import (
	"errors"
	"net/url"
)

// ErrBadRequest is the single error for any request that does not match an
// allow-listed shape. It never carries client input, so it can be echoed.
var ErrBadRequest = errors.New("invalid terrain request")

// ExactQuery accepts q only if it has exactly the keys in fixed (each with
// exactly that value) plus the keys in free, each key exactly once and with
// no other key. It returns the values of the free keys, which the caller
// must validate itself. Key matching is case-sensitive, so `TOKEN` or
// `Layers` are unknown keys.
func ExactQuery(q url.Values, fixed map[string]string, free ...string) (map[string]string, error) {
	if len(q) != len(fixed)+len(free) {
		return nil, ErrBadRequest
	}
	out := make(map[string]string, len(free))
	for _, k := range free {
		vs, ok := q[k]
		if !ok || len(vs) != 1 || vs[0] == "" {
			return nil, ErrBadRequest
		}
		out[k] = vs[0]
	}
	for k, vs := range q {
		if len(vs) != 1 {
			return nil, ErrBadRequest
		}
		if _, isFree := out[k]; isFree {
			continue
		}
		want, ok := fixed[k]
		if !ok || vs[0] != want {
			return nil, ErrBadRequest
		}
	}
	return out, nil
}
