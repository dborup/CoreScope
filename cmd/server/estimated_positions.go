package main

import (
	"bytes"
	"encoding/json"
	"net/http"
)

type EstimatedPositionsConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
}

type invalidEstimatedPositionsConfigError struct{ reason string }

func (e *invalidEstimatedPositionsConfigError) Error() string { return e.reason }

type EstimatedPositionsClientConfig struct {
	Enabled bool `json:"enabled"`
}

// Disabled analytics deliberately omit uncomputed measurements rather than
// report a misleading zero or "no neighbor evidence" conclusion.
type EstimatedPositionsDisabledResponse struct {
	EstimatedPositionsEnabled bool `json:"estimatedPositionsEnabled"`
}

type DisabledAreaAnalyticsResponse struct {
	EstimatedPositionsDisabledResponse
	Density           []AreaDensity    `json:"density"`
	BridgeNodes       []AreaBridgeNode `json:"bridgeNodes"`
	UnpositionedTotal int              `json:"unpositionedTotal"`
}

func (cfg *Config) estimatedPositionsEnabled() bool {
	return cfg == nil || cfg.EstimatedPositions == nil || cfg.EstimatedPositions.Enabled == nil || *cfg.EstimatedPositions.Enabled
}

func (s *Server) estimatedPositionsEnabled() bool {
	return !s.estimatedPositionsDisabled
}

// LoadConfig historically tolerates malformed JSON. A syntactically valid
// operator policy with a wrong type must instead fail startup explicitly:
// silently treating "false" as true would defeat the operator's intent.
func validateEstimatedPositionsConfig(data []byte) error {
	var envelope struct {
		EstimatedPositions json.RawMessage `json:"estimatedPositions"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil
	}
	if len(envelope.EstimatedPositions) == 0 {
		return nil
	}
	var block struct {
		Enabled json.RawMessage `json:"enabled"`
	}
	if bytes.Equal(bytes.TrimSpace(envelope.EstimatedPositions), []byte("null")) {
		return &invalidEstimatedPositionsConfigError{reason: "estimatedPositions must be an object"}
	}
	if err := json.Unmarshal(envelope.EstimatedPositions, &block); err != nil {
		return &invalidEstimatedPositionsConfigError{reason: "estimatedPositions must be an object"}
	}
	if len(block.Enabled) == 0 {
		return nil
	}
	raw := string(bytes.TrimSpace(block.Enabled))
	if raw != "true" && raw != "false" {
		return &invalidEstimatedPositionsConfigError{reason: "estimatedPositions.enabled must be a boolean"}
	}
	return nil
}

func (s *Server) estimateNodePosition(pubkey string) (string, float64, float64, int, float64, bool) {
	if !s.estimatedPositionsEnabled() {
		return "", 0, 0, 0, 0, false
	}
	return s.db.nearestPositionedNeighbor(pubkey, EstimateMaxEdgeKm)
}

func (s *Server) writeAreaAnalytics(w http.ResponseWriter, resp *AreaAnalyticsResponse) {
	if s.estimatedPositionsEnabled() {
		writeJSON(w, resp)
		return
	}
	writeJSON(w, DisabledAreaAnalyticsResponse{Density: resp.Density, BridgeNodes: resp.BridgeNodes, UnpositionedTotal: resp.UnpositionedTotal})
}

// stripEstimatedPositions only operates on request-owned paths (archived
// paths must be cloned first). Keep route identities, actual GPS and airtime.
// Distance is measured between observer endpoints, not relay coordinates:
// clearing an approximate relay must not erase a real endpoint measurement.
func stripEstimatedPositions(path *PacketPathResponse) {
	firstApprox := path.First != nil && path.First.Observer != nil && path.First.Observer.Approx
	strip := func(b *PacketPathBranch) {
		for i := range b.Points {
			p := &b.Points[i]
			if p.Approx {
				p.Lat, p.Lon, p.ApproxSpreadKm = nil, nil, nil
				p.Approx, p.ApproxNeighborCount = false, 0
			}
		}
		if firstApprox || (b.Observer != nil && b.Observer.Approx) {
			b.DistanceFromFirstKm = nil
		}
		if o := b.Observer; o != nil && o.Approx {
			o.Lat, o.Lon, o.ApproxSpreadKm = nil, nil, nil
			o.Approx, o.ApproxNeighborCount = false, 0
		}
	}
	for i := range path.Branches {
		strip(&path.Branches[i])
	}
	if path.First != nil {
		strip(path.First)
	}
	path.TouchedAreas = nil
}
