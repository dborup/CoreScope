package dhm

import "context"

// #941 M1 — a sub-budget of an ElevationSource. The shared Client has four
// upstream slots for every DHM feature together; the predicted reach
// fetches dozens of rasters per cold computation, so it goes through a
// LimitSource of at most two of them and the hillshade tiles and profiles
// keep the rest.

// LimitSource is src with at most n fetches in flight at once (n < 1 is
// 1). A fetch waiting for a slot stops when its context ends, with the
// context's FetchError category, and never reaches src.
func LimitSource(src ElevationSource, n int) ElevationSource {
	return &limitedSource{src: src, slots: make(chan struct{}, max(1, n))}
}

type limitedSource struct {
	src   ElevationSource
	slots chan struct{}
}

func (l *limitedSource) FetchRaster(ctx context.Context, r RasterRequest) (Grid, error) {
	select {
	case l.slots <- struct{}{}:
		defer func() { <-l.slots }()
	case <-ctx.Done():
		return Grid{}, ContextFailure(ctx.Err())
	}
	return l.src.FetchRaster(ctx, r)
}
