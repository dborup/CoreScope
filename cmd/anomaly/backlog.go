package main

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Ingest-backlog flag (#473, opt-in with -backlog-silence and
// -backlog-span; no defaults).
//
// An observation is a reconnect of its observer when that observer has no
// observation in the silence before it, [t - silence, t). Its observer's
// backlog span is [t, t + span). An event is flagged when its first
// observation (the one that sets the event's time) lies in a backlog span
// of its own observer. Times are observations.timestamp as stored, the
// time base of the #457 labels.
//
// The observer is read as observations.observer_idx and used only inside
// the export: a line gains "ingest_backlog": true and nothing else, and the
// output does not change when observers are renamed. Rows with a NULL
// observer are never flagged.
//
// Activity is one more index range scan on observations.timestamp, over
// [start - settle - silence - span, end + settle), under the same -max-rows
// cap. The first observation of an observer in that scan counts as a
// reconnect. That is exact for every row of the export query: a reconnect
// at r is real when r - silence lies inside the scan, and an earlier one
// has a span that ends before start - settle. It needs data over the whole
// scan, though: before the database's first observation every observer
// would look reconnected, so a scan that starts earlier is refused.

const (
	maxBacklogDuration = 24 * time.Hour
	// Capacity of the activity scan (not detection thresholds); exceeding
	// either fails the export.
	exportMaxObservers    = 1 << 16
	exportMaxBacklogSpans = 1 << 22
)

// backlogSummary counts the activity scan; it holds no values from the data.
type backlogSummary struct {
	// Rows is the observation rows of the activity scan.
	Rows int `json:"rows"`
	// RowsWithoutObserver counts rows whose observer_idx is NULL.
	RowsWithoutObserver int `json:"rows_without_observer"`
	// EventsFlagged counts written events with ingest_backlog set.
	EventsFlagged int `json:"events_flagged"`
}

func validBacklogDuration(d time.Duration) bool {
	return d > 0 && d <= maxBacklogDuration && d%time.Second == 0
}

func (o exportOptions) backlog() bool { return o.BacklogSilence != 0 || o.BacklogSpan != 0 }

func (o exportOptions) validateBacklog() error {
	if o.backlog() && (!validBacklogDuration(o.BacklogSilence) || !validBacklogDuration(o.BacklogSpan)) {
		return errors.New("-backlog-silence and -backlog-span go together, as whole seconds in (0, 24h]")
	}
	return nil
}

// firstObservationQuery is the time of the database's first observation.
const firstObservationQuery = `SELECT MIN(timestamp) FROM observations`

// backlogQuery selects the observer and time of every observation in
// [?1, ?2), in time order, at most ?3 rows.
const backlogQuery = `SELECT o.observer_idx, o.timestamp FROM observations o
WHERE o.timestamp >= ?1 AND o.timestamp < ?2
ORDER BY o.timestamp, o.id
LIMIT ?3`

type span struct{ from, to int64 } // [from, to), Unix seconds

type observerActivity struct {
	last  int64
	spans []span // merged, in time order
	next  int    // first span that may still contain a lookup
}

// backlogTracker holds the backlog spans of each observer, built by see
// in time order and read by flagged in time order.
type backlogTracker struct {
	silence, span          int64
	maxObservers, maxSpans int
	spans                  int
	obs                    map[int64]*observerActivity
}

func newBacklogTracker(silence, span int64, maxObservers, maxSpans int) *backlogTracker {
	return &backlogTracker{silence: silence, span: span, maxObservers: maxObservers, maxSpans: maxSpans,
		obs: make(map[int64]*observerActivity)}
}

// see records one observation of observer at ts (non-decreasing).
func (b *backlogTracker) see(observer, ts int64) error {
	a := b.obs[observer]
	switch {
	case a == nil:
		if len(b.obs) >= b.maxObservers {
			return fmt.Errorf("more than %d observers in the backlog activity scan", b.maxObservers)
		}
		a = &observerActivity{}
		b.obs[observer] = a
	case a.last >= ts-b.silence:
		a.last = ts
		return nil
	}
	a.last = ts
	if n := len(a.spans); n > 0 && a.spans[n-1].to >= ts {
		a.spans[n-1].to = ts + b.span
		return nil
	}
	if b.spans >= b.maxSpans {
		return fmt.Errorf("more than %d backlog spans in the activity scan", b.maxSpans)
	}
	b.spans++
	a.spans = append(a.spans, span{ts, ts + b.span})
	return nil
}

// flagged reports whether an observation of observer at ts (non-decreasing
// per observer) lies in one of its backlog spans.
func (b *backlogTracker) flagged(observer sql.NullInt64, ts int64) bool {
	if !observer.Valid {
		return false
	}
	a := b.obs[observer.Int64]
	if a == nil {
		return false
	}
	for a.next < len(a.spans) && a.spans[a.next].to <= ts {
		a.next++
	}
	return a.next < len(a.spans) && a.spans[a.next].from <= ts
}

// scanBacklog reads observer activity for the export range.
func scanBacklog(db *sql.DB, opt exportOptions, sum *backlogSummary) (*backlogTracker, error) {
	ok, err := hasColumn(db, "observations", "observer_idx")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("the backlog flag needs observations.observer_idx")
	}
	silence, spn := int64(opt.BacklogSilence/time.Second), int64(opt.BacklogSpan/time.Second)
	b := newBacklogTracker(silence, spn, exportMaxObservers, exportMaxBacklogSpans)
	lo := opt.Start.Add(-opt.Settle).Unix() - silence - spn
	var first sql.NullInt64
	if err := db.QueryRow(firstObservationQuery).Scan(&first); err != nil {
		return nil, err
	}
	if !first.Valid || first.Int64 > lo {
		return nil, fmt.Errorf("the backlog lookback starts at %s, before the database's first observation; "+
			"start later or shorten -backlog-silence/-backlog-span", time.Unix(lo, 0).UTC().Format(time.RFC3339))
	}
	rows, err := db.Query(backlogQuery, lo, opt.End.Add(opt.Settle).Unix(), opt.MaxRows+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		if sum.Rows++; sum.Rows > opt.MaxRows {
			return nil, fmt.Errorf("more than -max-rows %d observation rows in the backlog activity scan", opt.MaxRows)
		}
		var observer sql.NullInt64
		var ts int64
		if err := rows.Scan(&observer, &ts); err != nil {
			return nil, err
		}
		if !observer.Valid {
			sum.RowsWithoutObserver++
			continue
		}
		if err := b.see(observer.Int64, ts); err != nil {
			return nil, err
		}
	}
	return b, rows.Err()
}
