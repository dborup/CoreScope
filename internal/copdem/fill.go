package copdem

import (
	"context"
	"math"
	"sync"

	"github.com/meshcore-analyzer/dhm"
)

// #738 M1 — the fill of one terrain profile.
//
// The candidates are exactly the samples the plan does not cover
// (dhm.Plan.Inside false: foreign land and its coastal buffer, and
// everything outside DHM's raster extent); a covered sample is never
// filled, and DHM is never asked for an uncovered one (dhm.Plan.Fetch).
// Each candidate is projected back to lat/lon (dhm.FromUTM32): outside
// the tile list's region it has no value; on a tile the bucket does not
// have it is sea, 0 m, without a request; otherwise it is read from the
// block holding it, bilinearly (dhm.TiledLevel.Locate, dhm.Grid.Sample).
//
// The budget: one fill sends at most dhm.MaxCallsPerModel requests, the
// limit each DHM model already has. The cost of a level is every needed
// tile's cold header cost plus one call per needed block, counted the
// same whether or not anything is cached, so the level a path gets never
// depends on what happened to be in the cache. The full resolution (about
// 30 m) is taken when it fits; otherwise the ½ overview (about 60 m,
// #738 decision 7); otherwise there is no fill (ErrOverBudget).
//
// A fill is all or nothing: any failure returns no values, and the
// profile stays partial without a verdict (#738 decision 4).

// ErrOverBudget: the path needs more than dhm.MaxCallsPerModel upstream
// calls even at the ½ overview.
var ErrOverBudget = dhm.FetchError{Reason: "over budget"}

// Result is the fill of one plan.
type Result struct {
	// Values has one entry per plan sample: the GLO-30 height of a
	// sample the plan does not cover, NaN for a covered sample and for one
	// without a value (outside the region, or a nodata pixel).
	Values []float32
	// Level is the resolution read: 0 the full (1″ × 1.5″, about 30 m),
	// 1 the ½ overview (about 60 m).
	Level int
	// Filled is the number of samples given a value, sea included.
	Filled int
	// Calls is the level's cost against the budget: at most
	// dhm.MaxCallsPerModel, and never fewer than the requests sent.
	Calls int
}

// fillPoint is one candidate on a listed tile.
type fillPoint struct {
	i        int // plan sample
	lat, lon float64
	tile     int // index into the fill's tiles
}

// Fill fills plan's uncovered samples.
func (c *Client) Fill(ctx context.Context, plan *dhm.Plan) (Result, error) {
	values := make([]float32, len(plan.E))
	pts, tiles, sea := c.points(plan, values)
	res := Result{Values: values, Filled: sea}
	if len(tiles) == 0 {
		return res, nil
	}
	// A header costs at most two reads: refuse before reading any when
	// the headers alone could exceed the budget. An 80 km path touches at
	// most four tiles.
	if 2*len(tiles) > dhm.MaxCallsPerModel {
		return Result{}, ErrOverBudget
	}
	hs, err := c.readHeaders(ctx, tiles)
	if err != nil {
		return Result{}, err
	}
	for level := 0; level < 2; level++ {
		cost, groups := levelCost(level, pts, tiles, hs)
		if cost < 0 || cost > dhm.MaxCallsPerModel {
			continue
		}
		if err := c.sample(ctx, level, groups, pts, tiles, hs, values); err != nil {
			return Result{}, err
		}
		res.Level, res.Calls = level, cost
		for _, p := range pts {
			if !math.IsNaN(float64(values[p.i])) {
				res.Filled++
			}
		}
		return res, nil
	}
	return Result{}, ErrOverBudget
}

// points sets values to NaN, then to 0 for every candidate on a sea
// tile, and returns the candidates on listed tiles, the distinct listed
// tiles in path order, and the number of sea samples.
func (c *Client) points(plan *dhm.Plan, values []float32) ([]fillPoint, []Tile, int) {
	nan := float32(math.NaN())
	var pts []fillPoint
	var tiles []Tile
	sea := 0
	for i := range values {
		values[i] = nan
		if plan.Inside(i) {
			continue
		}
		lat, lon := dhm.FromUTM32(plan.E[i], plan.N[i])
		t, ok := TileAt(lat, lon)
		switch {
		case !ok || !t.InRegion():
			continue
		case !t.Listed():
			values[i] = 0
			sea++
			continue
		}
		// Consecutive samples are nearly always on one tile: try the last
		// one first. There are at most a handful.
		k := len(tiles) - 1
		if k < 0 || tiles[k] != t {
			k = indexOf(tiles, t)
			if k < 0 {
				tiles = append(tiles, t)
				k = len(tiles) - 1
			}
		}
		pts = append(pts, fillPoint{i: i, lat: lat, lon: lon, tile: k})
	}
	return pts, tiles, sea
}

func indexOf(tiles []Tile, t Tile) int {
	for k, x := range tiles {
		if x == t {
			return k
		}
	}
	return -1
}

// readHeaders reads every tile's header concurrently.
func (c *Client) readHeaders(ctx context.Context, tiles []Tile) ([]header, error) {
	hs := make([]header, len(tiles))
	err := all(ctx, len(tiles), func(ctx context.Context, k int) (err error) {
		hs[k], err = c.header(ctx, tiles[k])
		return err
	})
	return hs, err
}

// blockGroup is one block to read and the candidates it holds.
type blockGroup struct {
	tile, block int
	pts         []int // indices into the fill's points
}

// levelCost is the cost of reading the candidates at level, and the
// blocks it reads; -1 when a tile has no such level.
func levelCost(level int, pts []fillPoint, tiles []Tile, hs []header) (int, []blockGroup) {
	cost := 0
	for _, h := range hs {
		if level >= len(h.tiff.Levels) {
			return -1, nil
		}
		cost += h.calls
	}
	index := map[[2]int]int{} // (tile, block) → group; at most one per block on the path
	var groups []blockGroup
	for j, p := range pts {
		blk, _, _ := hs[p.tile].tiff.Levels[level].Locate(p.lon, p.lat)
		key := [2]int{p.tile, blk}
		g, ok := index[key]
		if !ok {
			g = len(groups)
			index[key] = g
			groups = append(groups, blockGroup{tile: p.tile, block: blk})
		}
		groups[g].pts = append(groups[g].pts, j)
	}
	return cost + len(groups), groups
}

// blockBuffers holds decode buffers of one full block (dhm.MaxTileSize²
// float32, 4 MiB), so a cold profile does not allocate one per block.
var blockBuffers = sync.Pool{New: func() any {
	b := make([]float32, dhm.MaxTileSize*dhm.MaxTileSize)
	return &b
}}

// sample reads the blocks of groups concurrently and writes each
// candidate's height into values. Groups hold disjoint candidates, so
// the writes never overlap.
func (c *Client) sample(ctx context.Context, level int, groups []blockGroup, pts []fillPoint, tiles []Tile, hs []header, values []float32) error {
	return all(ctx, len(groups), func(ctx context.Context, k int) error {
		g := groups[k]
		h := hs[g.tile]
		raw, err := c.block(ctx, tiles[g.tile], h, level, g.block)
		if err != nil {
			return err
		}
		// Decoding is bounded server-wide like the requests: at most
		// Concurrency blocks of 4 MiB are inflated at once.
		select {
		case c.decode <- struct{}{}:
			defer func() { <-c.decode }()
		case <-ctx.Done():
			return dhm.ContextFailure(ctx.Err())
		}
		buf := blockBuffers.Get().(*[]float32)
		defer blockBuffers.Put(buf)
		grid, err := decodeBlock(h.tiff, level, g.block, raw, *buf)
		if err != nil {
			return dhm.FetchError{Reason: "invalid block"}
		}
		lvl := &h.tiff.Levels[level]
		for _, j := range g.pts {
			p := pts[j]
			_, lon, lat := lvl.Locate(p.lon, p.lat)
			if v, ok := grid.Sample(lon, lat); ok {
				values[p.i] = float32(v)
			}
		}
		return nil
	})
}

// decodeBlock is dhm's block decoder; tests wrap it to watch how many
// blocks are decoded at once.
var decodeBlock = func(t *dhm.TiledGeoTIFF, level, i int, raw []byte, buf []float32) (dhm.Grid, error) {
	return t.DecodeBlock(level, i, raw, buf)
}

// all runs fn for 0..n-1 concurrently; the first error cancels the rest
// and is returned.
func all(ctx context.Context, n int, fn func(context.Context, int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
	)
	for k := 0; k < n; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			if err := fn(ctx, k); err != nil {
				once.Do(func() { first = err; cancel() })
			}
		}(k)
	}
	wg.Wait()
	return first
}
