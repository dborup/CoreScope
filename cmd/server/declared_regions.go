package main

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
)

// GET /api/nodes/{pubkey}/declared-regions (#126): the read-only consumer of
// the ingestor-owned client_declared_regions table. Contract and privacy
// rules: docs/client-regions.md.

// declaredRegionsAPILimit bounds one response to the newest reports.
const declaredRegionsAPILimit = 100

// DeclaredRegionsResponse is the endpoint's body.
type DeclaredRegionsResponse struct {
	Pubkey     string                  `json:"pubkey"`
	Reports    []DeclaredRegionsReport `json:"reports"`
	Collectors int                     `json:"collectors"` // distinct collectors among Reports
	Truncated  bool                    `json:"truncated"`  // older reports exist beyond the limit
}

// DeclaredRegionsReport is one stored report. The collector is an opaque
// number valid within one response; its key and position are never served,
// because key + target + time would place the contributor within direct
// range of a known repeater at a known moment.
type DeclaredRegionsReport struct {
	Collector     int      `json:"collector"`
	Regions       []string `json:"regions"` // [] = explicit empty answer
	Empty         bool     `json:"empty"`
	Truncated     bool     `json:"truncated"`
	ReportedAt    string   `json:"reported_at"`
	IngestedAt    string   `json:"ingested_at"`
	RepeaterClock *int64   `json:"repeater_clock"`
	HasPosition   bool     `json:"has_position"`
}

// declaredRegionsReports returns target's newest reports (reported_at
// descending, never id or ingested_at) and whether older ones exist. The
// UNIQUE(target_pubkey, reported_at, collector_pubkey) index serves the
// seek and the order. Before the ingestor has created the table it returns
// an empty list.
func (db *DB) declaredRegionsReports(target string, limit int) ([]DeclaredRegionsReport, int, bool, error) {
	reports := []DeclaredRegionsReport{}
	if !db.hasClientDeclaredRegions() {
		return reports, 0, false, nil
	}
	rows, err := db.conn.Query(`SELECT collector_pubkey, regions_csv, truncated, reported_at, ingested_at,
			repeater_clock, lat IS NOT NULL
		FROM client_declared_regions WHERE target_pubkey = ?
		ORDER BY reported_at DESC, collector_pubkey LIMIT ?`, target, limit+1)
	if err != nil {
		return nil, 0, false, err
	}
	defer rows.Close()
	labels := map[string]int{}
	for rows.Next() {
		var collector, csv string
		var r DeclaredRegionsReport
		var clock sql.NullInt64
		if err := rows.Scan(&collector, &csv, &r.Truncated, &r.ReportedAt, &r.IngestedAt, &clock, &r.HasPosition); err != nil {
			return nil, 0, false, err
		}
		if len(reports) == limit {
			return reports, len(labels), true, rows.Err()
		}
		if _, ok := labels[collector]; !ok {
			labels[collector] = len(labels) + 1
		}
		r.Collector = labels[collector]
		r.Regions = []string{}
		if csv != "" {
			r.Regions = strings.Split(csv, ",")
		}
		r.Empty = len(r.Regions) == 0
		if clock.Valid {
			v := clock.Int64
			r.RepeaterClock = &v
		}
		reports = append(reports, r)
	}
	return reports, len(labels), false, rows.Err()
}

func (s *Server) handleNodeDeclaredRegions(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.cfg == nil || !s.cfg.ClientRegionsEnabled() {
		http.NotFound(w, r)
		return
	}
	pubkey := strings.ToLower(mux.Vars(r)["pubkey"])
	if !isHexPubkey(pubkey) {
		http.Error(w, "invalid pubkey: expected 64 hex chars", http.StatusBadRequest)
		return
	}
	if s.cfg.IsBlacklisted(pubkey) || s.isPubkeyHidden(pubkey) {
		http.NotFound(w, r)
		return
	}
	if s.db == nil || s.db.conn == nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	reports, collectors, truncated, err := s.db.declaredRegionsReports(pubkey, declaredRegionsAPILimit)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, DeclaredRegionsResponse{Pubkey: pubkey, Reports: reports, Collectors: collectors, Truncated: truncated})
}
