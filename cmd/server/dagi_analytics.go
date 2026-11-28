package main

// #740 M3c — the Analytics page's DAGI notice.
//
// The Analytics endpoints filter by a DAGI selection through the node
// scope key (dagi_api.go) and answer in their own shapes, without the
// dagi.unpositionedHidden block that /api/nodes and /api/packets carry. The
// page reads the count from here instead: one small request per selection,
// answered from the membership snapshot the filters use (as fresh as the
// node snapshot, 30 s), with no DB query of its own and no change to any
// analytics response or cache key.

import "net/http"

// handleAnalyticsDagiUnpositioned is GET /api/analytics/dagi-unpositioned:
// how many visible nodes have no usable position, so no DAGI selection can
// include them. The count covers the whole node list and does not depend
// on the selection, so the endpoint takes no params. 404 with DAGI off.
func (s *Server) handleAnalyticsDagiUnpositioned(w http.ResponseWriter, r *http.Request) {
	var snap *dagiMembershipSnapshot
	if s.store != nil {
		snap = s.store.dagiSnapshot() // nil with DAGI off or the asset unusable
	}
	if snap == nil {
		writeError(w, http.StatusNotFound, "DAGI is off")
		return
	}
	writeJSON(w, DagiFilterInfo{UnpositionedHidden: snap.unpositionedVisible(s.cfg)})
}
