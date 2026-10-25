package main

import (
	"net/http"
	"time"
)

const (
	scopeTransportMinForwarded   = 5
	scopeTransportCacheMax       = 12
	scopeTransportMaxRegionCodes = 32
)

type ScopeTransportDeclaration struct {
	ConfiguredScope   string   `json:"configuredScope"`
	ConfiguredScopeAt string   `json:"configuredScopeAt"`
	DeclaredScopes    []string `json:"declaredScopes"`
	AllowsUnscoped    bool     `json:"allowsUnscoped"`
	StaleDeclaration  bool     `json:"staleDeclaration"`
	DeclaresScope     *bool    `json:"declaresScope"`
}
type ScopeTransportRow struct {
	PublicKey               string                     `json:"publicKey"`
	Name                    string                     `json:"name"`
	Role                    string                     `json:"role"`
	Declaration             *ScopeTransportDeclaration `json:"declaration"`
	Forwarded               int                        `json:"forwarded"`
	Carrier                 *int                       `json:"carrier"`
	NonCarrier              *int                       `json:"nonCarrier"`
	Unscoped                int                        `json:"unscoped"`
	UnknownScope            int                        `json:"unknownScope"`
	ObservedScopes          []ScopeAuditCount          `json:"observedScopes"`
	ObservedScopesTruncated bool                       `json:"observedScopesTruncated"`
	AmbiguousHops           int                        `json:"ambiguousHops"`
	AmbiguousScopeHops      *int                       `json:"ambiguousScopeHops"`
	LegacyObservations      int                        `json:"legacyObservations"`
	Evidence                string                     `json:"evidence"`
}
type ScopeTransportPopulation struct {
	Transmissions      int  `json:"transmissions"`
	Observations       int  `json:"observations"`
	ScopeTransmissions *int `json:"scopeTransmissions"`
	AmbiguousHops      int  `json:"ambiguousHops"`
	LegacyObservations int  `json:"legacyObservations"`
	RegionObservers    *int `json:"regionObservers"`
}
type ScopeTransportCoverage struct {
	OldestObservation *string `json:"oldestObservation"`
	Complete          bool    `json:"complete"`
}
type ScopeTransportSummary struct {
	Total           int `json:"total"`
	Carrier         int `json:"carrier"`
	Ambiguous       int `json:"ambiguous"`
	Incomplete      int `json:"incomplete"`
	LowEvidence     int `json:"lowEvidence"`
	NoScopeTraffic  int `json:"noScopeTraffic"`
	NotSeenCarrying int `json:"notSeenCarrying"`
	NeverQueried    int `json:"neverQueried"`
	Declared        int `json:"declared"`
}
type ScopeTransportResponse struct {
	Window          string                   `json:"window"`
	GeneratedAt     string                   `json:"generatedAt"`
	Since           string                   `json:"since"`
	Scope           string                   `json:"scope"`
	Region          []string                 `json:"region"`
	MinForwarded    int                      `json:"minForwarded"`
	Coverage        ScopeTransportCoverage   `json:"coverage"`
	Population      ScopeTransportPopulation `json:"population"`
	Summary         ScopeTransportSummary    `json:"summary"`
	Scopes          []ScopeAuditCount        `json:"scopes"`
	ScopesTruncated bool                     `json:"scopesTruncated"`
	Rows            []ScopeTransportRow      `json:"rows"`
	Truncated       bool                     `json:"truncated"`
}

type scopeTransportKey struct{ window, region string }
type scopeTransportAcc struct{}
type scopeTransportScan struct{ rows []scopeTransportAcc }

// Red stub (#414): replaced by the implementation in the next commit.
func classifyScopeTransport(forwarded, carrier, ambScope, unknown, legacy, scopeTraffic int) string {
	return ""
}

func (s *Server) buildScopeTransportScan(key scopeTransportKey, now time.Time) (*scopeTransportScan, error) {
	return &scopeTransportScan{}, nil
}

func (s *Server) handleScopeTransport(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, &ScopeTransportResponse{Rows: []ScopeTransportRow{}})
}
