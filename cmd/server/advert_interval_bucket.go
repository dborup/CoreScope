package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// The nodes of one advert interval distribution bucket (#896): GET
// /api/analytics/advert-intervals/bucket lists the nodes a Flood or
// Zero-hop bar of Analytics -> Advert Intervals counts.
//
//   - Membership is the histogram's own rule (advertIntervalBucketOf) over
//     the same snapshot rows and region/area filter, so a bar's count and
//     its list come from one data set.
//   - It reads the published recomputer snapshot only. Before the first
//     one it answers 503: a bucket request never scans the database (the
//     histogram's compute-on-miss fallback is deliberately not used).
//   - Identities are checked at serve time with the canonical rule
//     (identityHidden plus the bulk, alias-aware hiddenPubkeys lookup over
//     every member, not just one page). Hidden ones are withheld: counted
//     in withheld, never listed. The count stays the histogram's. A failed
//     lookup fails closed (500, no list).
//   - snapshot= names the snapshot the client paged from; once another is
//     published the answer is 409, so a list never mixes two snapshots.
//
// Cost: one pass over the snapshot rows, O(N), a sort of the K members,
// O(K log K), and with hidden-name prefixes configured one bulk
// primary-key lookup of the K keys. No store lock: the rows of a published
// snapshot are never written.
//
// The page sizes are hardcoded (#896 owner decision: not a customizer
// item).

const (
	advertIntervalBucketDefaultLimit = 50
	advertIntervalBucketMaxLimit     = 200
)

// AdvertIntervalBucketResponse is GET /api/analytics/advert-intervals/bucket.
type AdvertIntervalBucketResponse struct {
	Class       string `json:"class"`
	IntervalS   int64  `json:"interval_s"`
	Snapped     bool   `json:"snapped"`
	GeneratedAt string `json:"generated_at"` // scan.generated_at of the snapshot served
	// Nodes is the bucket's count in this snapshot and filter: the
	// histogram's. Visible members are listed, Withheld (Nodes - Visible)
	// are hidden identities.
	Nodes    int                          `json:"nodes"`
	Visible  int                          `json:"visible"`
	Withheld int                          `json:"withheld"`
	Offset   int                          `json:"offset"`
	Limit    int                          `json:"limit"`
	Members  []AdvertIntervalBucketMember `json:"members"`
}

// AdvertIntervalBucketMember is one listed node and its own estimate.
type AdvertIntervalBucketMember struct {
	PublicKey string   `json:"public_key"`
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	Regions   []string `json:"regions"`
	// IntervalS is the node's estimate; in an unsnapped bucket it is not
	// rounded like the bucket's interval_s.
	IntervalS  int64  `json:"interval_s"`
	Snapped    bool   `json:"snapped"`
	Confidence string `json:"confidence"`
	Samples    int    `json:"samples"`
	GapsUsed   int    `json:"gaps_used"`
}

// AdvertIntervalBucketConflict is the 409 body: the snapshot= the client
// paged from is no longer the published one.
type AdvertIntervalBucketConflict struct {
	Error       string `json:"error"`
	GeneratedAt string `json:"generated_at"`
}

// advertIntervalBucketQuery is a validated request.
type advertIntervalBucketQuery struct {
	class         advertIntervalClass
	className     string
	key           advertIntervalBucketKey
	region, area  string
	snapshot      string
	offset, limit int
}

// allDigits reports whether s is only ASCII digits: strconv also accepts a
// leading "+" (and "-"), which the endpoint's validation table rejects.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseAdvertIntervalBucketQuery validates the parameters; msg is the 400
// message when they are not valid. interval_s has no upper bound: an
// unsnapped estimate can lie above the longest setting.
func parseAdvertIntervalBucketQuery(v url.Values) (q advertIntervalBucketQuery, msg string) {
	switch q.className = v.Get("class"); q.className {
	case advertClassFlood:
		q.class = advertIntervalFlood
	case advertClassZeroHop:
		q.class = advertIntervalZeroHop
	default:
		return q, "class must be flood or zero_hop"
	}
	n, err := strconv.ParseInt(v.Get("interval_s"), 10, 64)
	if err != nil || n < 1 || !allDigits(v.Get("interval_s")) {
		return q, "interval_s must be a positive integer"
	}
	q.key.v = n
	switch v.Get("snapped") {
	case "true":
		q.key.snapped = true
	case "false":
	default:
		return q, "snapped must be true or false"
	}
	q.limit = advertIntervalBucketDefaultLimit
	if s := v.Get("limit"); s != "" {
		if q.limit, err = strconv.Atoi(s); err != nil || q.limit < 1 || !allDigits(s) {
			return q, "limit must be a positive integer"
		}
		q.limit = min(q.limit, advertIntervalBucketMaxLimit)
	}
	if s := v.Get("offset"); s != "" {
		if q.offset, err = strconv.Atoi(s); err != nil || q.offset < 0 || !allDigits(s) {
			return q, "offset must be a non-negative integer"
		}
	}
	q.region, q.area, q.snapshot = v.Get("region"), v.Get("area"), v.Get("snapshot")
	return q, ""
}

// advertIntervalBucketMembers returns the rows keep accepts whose class
// estimate is in bucket key, in list order: name case-insensitively, then
// name, then key - a total order, so offset paging is stable within one
// snapshot. Pure: no store, lock or database.
func advertIntervalBucketMembers(rows []advertIntervalNodeRow, keep func(*advertIntervalNodeRow) bool, class advertIntervalClass, key advertIntervalBucketKey) []*advertIntervalNodeRow {
	type member struct {
		row   *advertIntervalNodeRow
		lower string
	}
	var ms []member
	for i := range rows {
		r := &rows[i]
		if k, ok := advertIntervalBucketOf(r.estimate(class), class); ok && k == key && keep(r) {
			ms = append(ms, member{r, strings.ToLower(r.name)})
		}
	}
	sort.Slice(ms, func(i, j int) bool {
		a, b := ms[i], ms[j]
		if a.lower != b.lower {
			return a.lower < b.lower
		}
		if a.row.name != b.row.name {
			return a.row.name < b.row.name
		}
		return a.row.pubkey < b.row.pubkey
	})
	out := make([]*advertIntervalNodeRow, len(ms))
	for i, m := range ms {
		out[i] = m.row
	}
	return out
}

// advertIntervalBucketPage is the response for a bucket's members with the
// hidden keys withheld: the counts cover every member, the page the
// visible ones from q.offset.
func advertIntervalBucketPage(members []*advertIntervalNodeRow, hidden map[string]bool, q advertIntervalBucketQuery, generatedAt string) AdvertIntervalBucketResponse {
	resp := AdvertIntervalBucketResponse{Class: q.className, IntervalS: q.key.v, Snapped: q.key.snapped, GeneratedAt: generatedAt,
		Nodes: len(members), Offset: q.offset, Limit: q.limit, Members: []AdvertIntervalBucketMember{}}
	for _, r := range members {
		if hidden[r.pubkey] {
			resp.Withheld++
			continue
		}
		if resp.Visible >= q.offset && len(resp.Members) < q.limit {
			e := r.estimate(q.class)
			regions := r.regions
			if regions == nil {
				regions = []string{}
			}
			resp.Members = append(resp.Members, AdvertIntervalBucketMember{PublicKey: r.pubkey, Name: r.name, Role: r.role, Regions: regions,
				IntervalS: *e.IntervalS, Snapped: e.Snapped, Confidence: e.Confidence, Samples: e.Samples, GapsUsed: e.GapsUsed})
		}
		resp.Visible++
	}
	return resp
}

// advertIntervalIdentity is a key and the name a response would show for it.
type advertIntervalIdentity struct{ pubkey, name string }

// hiddenAdvertIntervalKeys returns the lower-case keys among ids whose
// identity is hidden now: identityHidden applied to the name the response
// would show (fail closed when the snapshot's name is older than the live
// one) and, in one bulk query, to every live name of the key - node,
// inactive node and observer aliases in any case (hiddenPubkeys). A
// snapshot pass only left out identities hidden by node name, so one hidden
// since, or only through an alias, is caught here. nil without any hiding
// rule configured, at no query.
func (s *Server) hiddenAdvertIntervalKeys(ctx context.Context, ids []advertIntervalIdentity) (map[string]bool, error) {
	if !s.routePatternsRestricted() {
		return nil, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = id.pubkey
	}
	hidden, err := s.hiddenPubkeys(ctx, keys)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if identityHidden(s.cfg, id.pubkey, id.name) {
			hidden[strings.ToLower(id.pubkey)] = true
		}
	}
	return hidden, nil
}

// visibleAdvertIntervalOutliers withholds the outliers whose identity is
// hidden (hiddenAdvertIntervalKeys), counting them in OutliersWithheld;
// outliers_total and the distributions are left as they are. resp may hold
// the snapshot's own slice, so a filtered list is a new slice.
func (s *Server) visibleAdvertIntervalOutliers(ctx context.Context, resp AdvertIntervalAnalyticsResponse) (AdvertIntervalAnalyticsResponse, error) {
	ids := make([]advertIntervalIdentity, len(resp.Outliers))
	for i, o := range resp.Outliers {
		ids[i] = advertIntervalIdentity{o.PublicKey, o.Name}
	}
	hidden, err := s.hiddenAdvertIntervalKeys(ctx, ids)
	if err != nil || len(hidden) == 0 {
		return resp, err
	}
	kept := make([]AdvertIntervalOutlier, 0, len(resp.Outliers))
	for _, o := range resp.Outliers {
		if hidden[strings.ToLower(o.PublicKey)] {
			resp.OutliersWithheld++
			continue
		}
		kept = append(kept, o)
	}
	resp.Outliers = kept
	return resp, nil
}

// handleAnalyticsAdvertIntervalBucket serves GET
// /api/analytics/advert-intervals/bucket (see the file comment).
func (s *Server) handleAnalyticsAdvertIntervalBucket(w http.ResponseWriter, r *http.Request) {
	q, msg := parseAdvertIntervalBucketQuery(r.URL.Query())
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if s.rejectAnalyticsFilters(w, q.region, q.area) {
		return
	}
	var snap *advertIntervalSnapshot
	if s.store != nil {
		rc := s.store.recomputer(&s.store.recompAdvertIntervals)
		setAnalyticsSnapshotHeaders(w, rc) // #492, before the read
		if rc != nil {
			snap, _ = rc.Load().(*advertIntervalSnapshot)
		}
	}
	if snap == nil {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "advert interval analytics are warming up")
		return
	}
	generatedAt := snap.all.Scan.GeneratedAt
	if q.snapshot != "" && q.snapshot != generatedAt {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(AdvertIntervalBucketConflict{Error: "the analytics snapshot changed", GeneratedAt: generatedAt})
		return
	}
	var areaNodes map[string]bool
	if q.area != "" {
		areaNodes = s.store.resolveAreaNodes(q.area)
	}
	members := advertIntervalBucketMembers(snap.rows, advertIntervalFilter(q.region, areaNodes), q.class, q.key)
	ids := make([]advertIntervalIdentity, len(members))
	for i, m := range members {
		ids[i] = advertIntervalIdentity{m.pubkey, m.name}
	}
	hidden, err := s.hiddenAdvertIntervalKeys(r.Context(), ids)
	if err != nil {
		log.Printf("[advert-intervals] bucket identity metadata: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read identity metadata")
		return
	}
	// Visibility is evaluated on every serve; no shared cache may keep it.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, advertIntervalBucketPage(members, hidden, q, generatedAt))
}
