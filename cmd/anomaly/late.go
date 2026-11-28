package main

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Late-delivery flag (#538, opt-in with -late-window, -late-min-rows,
// -late-share and -late-gap; no defaults). It writes the same
// "ingest_backlog": true as the #473 flag, so a rate rule's
// exclude_ingest_backlog guard reads it unchanged; the two flags exclude
// each other.
//
// It marks the events of an observer burst that delivers transmissions
// other observers heard minutes earlier: queued packets stamped when the
// observer's uploader finally sent them, not when they were heard. The #473
// flag (first observations after a silence) also marked a real storm that
// an often-quiet observer heard first; this one needs the burst to repeat
// what the rest of the network already reported.
//
//   - Delay. For a row r of observer O at time t, e is the latest
//     observation of the same transmission by another (non-NULL) observer
//     before t + gap, over the whole database. r is already seen when there
//     is one and d = t - e >= gap: other observers heard the transmission
//     at least gap before r and none of them within gap of r. A live row
//     has a live peer within seconds; one observer whose clock runs early
//     does not make the others look late.
//   - Window. Bins are epoch-aligned, -late-window wide. The window of
//     (O, bin k) is O's rows in bins k-1, k and k+1.
//   - Burst. (O, k) is a late burst when its window holds at least
//     -late-min-rows rows, at least -late-share of them are already seen,
//     and the delays of those span at least gap (max d - min d). The spread
//     separates a queue, which holds transmissions heard over the whole
//     outage, from an observer whose clock runs behind by a constant
//     offset: every one of its delays is about that offset.
//   - Flag. An event is flagged when its first observation (the one that
//     sets the event's time) is a row of O in a late burst (O, its bin).
//     Rows with a NULL observer are never flagged.
//
// Limits (README, late_limits_538_test.go): observers that drain a common
// outage together are each other's peer and are never flagged; the share
// counts queue-only rows, so a large queue after a short outage is missed;
// two clocks behind by different offsets can pass the spread at any share.
//
// The observer is read as observations.observer_idx and used only inside
// the export, as a map key and for "another observer"; only the boolean
// leaves, so the output does not change when observers are renamed.
//
// Activity is one more index range scan on observations.timestamp over
// [start - settle - 2*window, end + settle + 2*window), which holds every
// window of a row the export reads, under the same -max-rows cap; per row
// one index search over the observations of its transmission. Memory is
// one entry per (observer, bin) with rows, capped.

const (
	maxLateDuration = time.Hour
	maxLateMinRows  = 1 << 20
	// Capacity of the activity scan (not a detection threshold); exceeding
	// it fails the export.
	exportMaxLateBins = 1 << 22
)

// lateSummary counts the activity scan; it holds no values from the data.
type lateSummary struct {
	// Rows is the observation rows of the activity scan.
	Rows int `json:"rows"`
	// RowsWithoutObserver counts rows whose observer_idx is NULL.
	RowsWithoutObserver int `json:"rows_without_observer"`
	// RowsAlreadySeen counts rows whose delay is at least -late-gap.
	RowsAlreadySeen int `json:"rows_already_seen"`
	// EventsFlagged counts written events with ingest_backlog set.
	EventsFlagged int `json:"events_flagged"`
}

func validLateDuration(d time.Duration) bool {
	return d > 0 && d <= maxLateDuration && d%time.Second == 0
}

func (o exportOptions) late() bool {
	return o.LateWindow != 0 || o.LateMinRows != 0 || o.LateShare != 0 || o.LateGap != 0
}

func (o exportOptions) validateLate() error {
	switch {
	case !o.late():
		return nil
	case o.backlog():
		return errors.New("the -backlog-* and -late-* flags exclude each other")
	case !validLateDuration(o.LateWindow) || !validLateDuration(o.LateGap) ||
		o.LateMinRows < 1 || o.LateMinRows > maxLateMinRows || !(o.LateShare > 0 && o.LateShare <= 1):
		return fmt.Errorf("-late-window, -late-min-rows, -late-share and -late-gap go together: "+
			"whole seconds in (0, 1h], 1..%d rows and a share in (0, 1]", maxLateMinRows)
	}
	return nil
}

// lateQuery selects the observer, time and elsewhere time e (NULL when
// there is none) of every observation in [?1, ?2), in time order, at most
// ?3 rows; ?4 is the gap in seconds. "<>" is never true for a NULL
// observer, so rows without one are never "another observer".
const lateQuery = `SELECT o.observer_idx, o.timestamp,
  (SELECT MAX(p.timestamp) FROM observations p
   WHERE p.transmission_id = o.transmission_id AND p.timestamp < o.timestamp + ?4
     AND p.observer_idx <> o.observer_idx)
FROM observations o
WHERE o.timestamp >= ?1 AND o.timestamp < ?2
ORDER BY o.timestamp, o.id
LIMIT ?3`

type lateKey struct{ observer, bin int64 }

// lateBin counts one observer's rows in one bin; dmin and dmax are the
// least and greatest delay of its already-seen rows (when seen > 0).
type lateBin struct {
	rows, seen int
	dmin, dmax int64
}

func (b *lateBin) add(o lateBin) {
	if o.seen > 0 {
		if b.seen == 0 || o.dmin < b.dmin {
			b.dmin = o.dmin
		}
		if b.seen == 0 || o.dmax > b.dmax {
			b.dmax = o.dmax
		}
	}
	b.rows += o.rows
	b.seen += o.seen
}

// lateTracker holds the bins of each observer, built by see and read by
// flagged.
type lateTracker struct {
	window, gap int64
	minRows     int
	share       float64
	maxBins     int
	bins        map[lateKey]*lateBin
}

func newLateTracker(opt exportOptions, maxBins int) *lateTracker {
	return &lateTracker{window: int64(opt.LateWindow / time.Second), gap: int64(opt.LateGap / time.Second),
		minRows: opt.LateMinRows, share: opt.LateShare, maxBins: maxBins, bins: make(map[lateKey]*lateBin)}
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// see records one row of observer at ts; elsewhere is the latest
// observation of its transmission by another observer before ts + gap. It
// reports whether the row is already seen.
func (l *lateTracker) see(observer, ts int64, elsewhere sql.NullInt64) (bool, error) {
	k := lateKey{observer, floorDiv(ts, l.window)}
	b := l.bins[k]
	if b == nil {
		if len(l.bins) >= l.maxBins {
			return false, fmt.Errorf("more than %d (observer, bin) entries in the late-delivery activity scan", l.maxBins)
		}
		b = &lateBin{}
		l.bins[k] = b
	}
	r := lateBin{rows: 1}
	if d := ts - elsewhere.Int64; elsewhere.Valid && d >= l.gap {
		r.seen, r.dmin, r.dmax = 1, d, d
	}
	b.add(r)
	return r.seen == 1, nil
}

// flagged reports whether an observation of observer at ts lies in a late
// burst of that observer. The share bound is in floating point: exact for
// shares that are multiples of 1/2^k (0.5), while for example 0.07 * 100
// rounds above 7.
func (l *lateTracker) flagged(observer sql.NullInt64, ts int64) bool {
	if !observer.Valid {
		return false
	}
	k := floorDiv(ts, l.window)
	var w lateBin
	for i := k - 1; i <= k+1; i++ {
		if b := l.bins[lateKey{observer.Int64, i}]; b != nil {
			w.add(*b)
		}
	}
	return w.rows >= l.minRows && float64(w.seen) >= l.share*float64(w.rows) &&
		w.seen > 0 && w.dmax-w.dmin >= l.gap
}

// scanLate reads observer activity for the export range.
func scanLate(db *sql.DB, opt exportOptions, sum *lateSummary) (*lateTracker, error) {
	ok, err := hasColumn(db, "observations", "observer_idx")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("the late-delivery flag needs observations.observer_idx")
	}
	l := newLateTracker(opt, exportMaxLateBins)
	margin := opt.Settle + 2*opt.LateWindow
	rows, err := db.Query(lateQuery, opt.Start.Add(-margin).Unix(), opt.End.Add(margin).Unix(), opt.MaxRows+1, l.gap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		if sum.Rows++; sum.Rows > opt.MaxRows {
			return nil, fmt.Errorf("more than -max-rows %d observation rows in the late-delivery activity scan", opt.MaxRows)
		}
		var observer, elsewhere sql.NullInt64
		var ts int64
		if err := rows.Scan(&observer, &ts, &elsewhere); err != nil {
			return nil, err
		}
		if !observer.Valid {
			sum.RowsWithoutObserver++
			continue
		}
		seen, err := l.see(observer.Int64, ts, elsewhere)
		if err != nil {
			return nil, err
		}
		if seen {
			sum.RowsAlreadySeen++
		}
	}
	return l, rows.Err()
}
