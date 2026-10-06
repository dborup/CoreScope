package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
)

// PingScorePathResponse distinguishes recorded evidence from today's live
// geometry and from an old record whose observations expired before capture.
// An unavailable route is not an empty map, nor evidence of zero reception.
type PingScorePathResponse struct {
	Status     string              `json:"status"`
	CapturedAt string              `json:"capturedAt,omitempty"`
	Reason     string              `json:"reason,omitempty"`
	Path       *PacketPathResponse `json:"path,omitempty"`
}

func (s *Server) handlePingScorePath(w http.ResponseWriter, r *http.Request) {
	// Paths are bounded to the ten currently displayed record slots, not a
	// second general-purpose packet API or an archive of all historical pings.
	key := r.URL.Query().Get("record")
	if !validPingScoreRecordKey(key) {
		writeError(w, http.StatusBadRequest, "invalid ping record slot")
		return
	}
	hash := mux.Vars(r)["hash"]
	snap := s.pingScores.Load()
	if snap == nil {
		writeJSON(w, PingScorePathResponse{Status: "initializing"})
		return
	}
	score := pingScoreRecordSlots(snap)[key]
	if score == nil || !strings.EqualFold(score.Hash, hash) {
		writeError(w, http.StatusNotFound, "ping record is no longer in this slot")
		return
	}
	result := PingScorePathResponse{Status: "live"}
	var path *PacketPathResponse
	// Prefer the evidence captured with this card's metrics. In particular,
	// current GPS loss must not turn a retained distance into a smaller map.
	if a, ok := snap.pathArchives[key]; ok && a.Hash == score.Hash && a.Timestamp == score.Timestamp {
		path = &a.Path
		result.Status, result.CapturedAt = "archived", a.CapturedAt
	} else if s.db != nil {
		var err error
		path, err = s.db.GetPacketPath(score.Hash, EstimateMaxEdgeKm)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not load ping record path")
			return
		}
		s.annotatePacketPathAirtime(path)
		if path != nil && len(path.Branches) > 0 && !pingPathMatchesRecord(key, score, path) {
			writeJSON(w, PingScorePathResponse{Status: "unavailable", Reason: "record_evidence_unavailable"})
			return
		}
	}
	if path == nil || len(path.Branches) == 0 {
		writeJSON(w, PingScorePathResponse{Status: "unavailable", Reason: "raw_data_expired_before_capture"})
		return
	}
	data, err := boundedPingPathJSON(path)
	if err != nil {
		writeJSON(w, PingScorePathResponse{Status: "unavailable", Reason: "archive_too_large"})
		return
	}
	// Snapshot archives are immutable and shared with the history worker.
	// Clone before per-request filtering; never change their cached geometry.
	var visible PacketPathResponse
	if err := json.Unmarshal(data, &visible); err != nil {
		writeError(w, http.StatusInternalServerError, "could not read ping record path")
		return
	}
	filtered, err := s.filterPingScorePath(r.Context(), &visible)
	if err != nil {
		// A failed live name lookup cannot turn cached private names public.
		writeError(w, http.StatusInternalServerError, "could not check ping path visibility")
		return
	}
	if len(visible.Branches) == 0 || !pingScorePathHasCoordinates(&visible) {
		reason := "no_coordinates"
		if filtered {
			reason = "privacy_filtered"
		}
		writeJSON(w, PingScorePathResponse{Status: "unavailable", Reason: reason})
		return
	}
	s.annotatePacketPathTouchedAreas(&visible)
	result.Path = &visible
	writeJSON(w, result)
}

func pingScorePathHasCoordinates(path *PacketPathResponse) bool {
	positioned := func(b PacketPathBranch) bool {
		if b.Observer != nil && b.Observer.Lat != nil && b.Observer.Lon != nil {
			return true
		}
		for _, p := range b.Points {
			if p.Lat != nil && p.Lon != nil {
				return true
			}
		}
		return false
	}
	for _, b := range path.Branches {
		if positioned(b) {
			return true
		}
	}
	return path.First != nil && positioned(*path.First)
}

// filterPingScorePath applies the shared identity policy to recorded names
// AND current node/observer/inactive names. Entire hidden branches disappear:
// removing just a point would draw a fictitious edge across the hidden hop.
func (s *Server) filterPingScorePath(ctx context.Context, path *PacketPathResponse) (bool, error) {
	keys := make([]string, 0)
	collect := func(b PacketPathBranch) {
		for _, p := range b.Points {
			keys = append(keys, p.PublicKey)
		}
		if b.Observer != nil {
			keys = append(keys, b.Observer.PublicKey)
		}
	}
	for _, b := range path.Branches {
		collect(b)
	}
	if path.First != nil {
		collect(*path.First)
	}
	var names map[string][]string
	var err error
	if s.cfg != nil {
		names, err = s.hiddenIdentityNames(ctx, keys)
		if err != nil {
			return false, err
		}
	}
	hidden := func(pk, recorded string) bool {
		return identityHidden(s.cfg, pk, recorded) || identityHidden(s.cfg, pk, names[strings.ToLower(pk)]...)
	}
	branchHidden := func(b PacketPathBranch) bool {
		if b.Observer != nil && hidden(b.Observer.PublicKey, b.Observer.Name) {
			return true
		}
		for _, p := range b.Points {
			if hidden(p.PublicKey, p.Name) {
				return true
			}
		}
		return false
	}
	filtered := false
	kept := path.Branches[:0]
	for _, b := range path.Branches {
		if branchHidden(b) {
			filtered = true
			continue
		}
		kept = append(kept, b)
	}
	path.Branches = kept
	if path.First != nil && branchHidden(*path.First) {
		path.First = nil
		filtered = true
		// These measurements were relative to a now-hidden origin.
		for i := range path.Branches {
			path.Branches[i].DistanceFromFirstKm = nil
			path.Branches[i].SecondsAfterFirst = nil
		}
	}
	// Neighbor-centroid positions have no contributor identities in the path
	// contract. With a visibility policy we cannot prove their contributors
	// are public, so omit this geometry rather than leak a hidden location.
	restricted := s.cfg != nil && (s.cfg.HasNodeBlacklist() || len(s.cfg.ObserverBlacklist) > 0 || len(s.cfg.EnforcedHiddenNamePrefixes()) > 0)
	if restricted {
		stripApprox := func(b *PacketPathBranch) {
			for i := range b.Points {
				p := &b.Points[i]
				if p.Approx {
					p.Lat, p.Lon, p.ApproxSpreadKm = nil, nil, nil
					p.ApproxNeighborCount = 0
					filtered = true
				}
			}
			if o := b.Observer; o != nil && o.Approx {
				o.Lat, o.Lon, o.ApproxSpreadKm = nil, nil, nil
				o.ApproxNeighborCount = 0
				filtered = true
			}
		}
		for i := range path.Branches {
			stripApprox(&path.Branches[i])
		}
		if path.First != nil {
			stripApprox(path.First)
		}
	}
	path.TouchedAreas = nil // recomputed from visible geometry and current config
	if filtered {
		path.EstimatedAirtimeMs, path.AirtimeRelayCount = nil, 0
	}
	return filtered, nil
}
