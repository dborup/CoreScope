package main

import (
	"math"
	"time"
	"unsafe"
)

// Retained-heap accounting for the published Regions snapshot (#508).
//
// Publishing the route/membership snapshot raised staging container RSS from
// about 1.55 to about 1.72 GiB, and production hosts are smaller (one has
// 3.7 GiB shared with other services), so the retained size has to be an
// operator diagnostic rather than a guess.
//
// mapEntryBytes is a flat per-entry allowance for a Go map bucket slot and its
// load factor; sliceHeaderBytes covers one slice header. Like the store
// breakdown these are deliberate estimates: struct sizes plus owned string
// content plus a fixed per-entry allowance, not allocator measurements. Keys
// and scope names are shared with the packet store and the prefix map, so
// counting their content here is an upper bound.
const (
	mapEntryBytes    = 48
	sliceHeaderBytes = 24
)

// RegionsSnapshotMemory sizes the retained Regions route, membership, hourly
// history and per-bucket evidence published with the shared relay snapshot. It
// walks only immutable published state: no SQL, no packet-store walk and no
// packet lock (#495).
type RegionsSnapshotMemory struct {
	GeneratedAt string `json:"generatedAt"`
	// Published counts, so a byte figure can be read against the budgets.
	Nodes         int `json:"nodes"`
	Scopes        int `json:"scopes"`
	HistoryNodes  int `json:"historyNodes"`
	HistoryCells  int `json:"historyCells"`
	Steps         int `json:"steps"`
	StepCells     int `json:"stepCells"`
	EvidenceCells int `json:"evidenceCells"`

	MembershipsBytes int64 `json:"membershipsBytes"`
	HistoryBytes     int64 `json:"historyBytes"`
	StepsBytes       int64 `json:"stepsBytes"`
	EvidenceBytes    int64 `json:"evidenceBytes"`
	TotalBytes       int64 `json:"totalBytes"`
	// TotalMB is TotalBytes in MiB, two decimals, matching the store breakdown.
	TotalMB float64 `json:"totalMB"`
	// EstimatedStepPayloadBytes is the published steps array measured with the
	// same row/cell estimate the directed-step byte budget spends, so an
	// operator can see how much of maxRegionsRouteStepBytes a snapshot used.
	EstimatedStepPayloadBytes int64 `json:"estimatedStepPayloadBytes"`
	StepPayloadBudgetBytes    int64 `json:"stepPayloadBudgetBytes"`
	// Truncated repeats the snapshot's own publication flags, so a small byte
	// figure is not mistaken for a complete snapshot.
	Truncated bool `json:"truncated"`
}

func stringBytes(s string) int64 { return int64(len(s)) + strHdr }

func regionsHistoryBucketsBytes(buckets []RegionsHistoryBucket) int64 {
	total := sliceHeaderBytes + int64(len(buckets))*int64(unsafe.Sizeof(RegionsHistoryBucket{}))
	for _, bucket := range buckets {
		total += int64(len(bucket.LastRelayed))
	}
	return total
}

// measureRegionsSnapshot is O(published cells) and allocation-free. The
// snapshot is immutable once published, so the caller may release
// repeaterEnrichMu before calling.
func measureRegionsSnapshot(snapshot *repeaterRelaySnapshot) *RegionsSnapshotMemory {
	out := &RegionsSnapshotMemory{GeneratedAt: snapshot.generatedAt.Format(time.RFC3339Nano),
		StepPayloadBudgetBytes: maxRegionsRouteStepBytes}
	if m := snapshot.regionMemberships; m != nil {
		for key, info := range m.nodes {
			out.Nodes++
			out.MembershipsBytes += stringBytes(key) + mapEntryBytes +
				int64(unsafe.Sizeof(regionsNodeMembership{})) + sliceHeaderBytes
			for _, scope := range info.scopes {
				out.Scopes++
				out.MembershipsBytes += stringBytes(scope.name) + int64(unsafe.Sizeof(regionScopeRelay{}))
			}
		}
		if e := m.evidence; e != nil {
			out.EvidenceBytes += sliceHeaderBytes
			for _, scope := range e.scopes {
				out.EvidenceBytes += stringBytes(scope.Name) + int64(unsafe.Sizeof(RegionsEvidenceScope{})) + sliceHeaderBytes
				out.EvidenceCells += len(scope.Buckets)
				out.EvidenceBytes += int64(len(scope.Buckets)) * int64(unsafe.Sizeof(RegionsEvidenceBucket{}))
			}
			// The precomputed history summary is retained alongside the cells.
			for _, scope := range e.history.Scopes {
				out.EvidenceBytes += stringBytes(scope.Name) + int64(unsafe.Sizeof(RegionsEvidenceScope{})) +
					sliceHeaderBytes + int64(len(scope.Buckets))*int64(unsafe.Sizeof(RegionsEvidenceBucket{}))
			}
		}
	}
	if h := snapshot.history; h != nil {
		out.Truncated = out.Truncated || h.truncated || h.scopesTruncated || h.bucketsTruncated
		for key, scopes := range h.nodes {
			out.HistoryNodes++
			out.HistoryBytes += stringBytes(key) + mapEntryBytes + sliceHeaderBytes
			for _, scope := range scopes {
				out.HistoryBytes += stringBytes(scope.Name) + int64(unsafe.Sizeof(RegionsHistoryScope{})) +
					regionsHistoryBucketsBytes(scope.Buckets)
				out.HistoryCells += len(scope.Buckets)
			}
		}
	}
	if r := snapshot.routes; r != nil {
		out.Truncated = out.Truncated || r.stepsTruncated || r.bucketsTruncated || r.evidence.Incomplete
		out.StepsBytes += sliceHeaderBytes
		for _, step := range r.steps {
			out.Steps++
			out.StepCells += len(step.Buckets)
			out.StepsBytes += stringBytes(step.From) + stringBytes(step.To) + stringBytes(step.Scope) +
				int64(unsafe.Sizeof(RegionsRouteStep{})) + regionsHistoryBucketsBytes(step.Buckets)
		}
		out.EstimatedStepPayloadBytes = int64(out.Steps)*regionsRouteStepRowBytes + int64(out.StepCells)*regionsRouteStepCellBytes
	}
	out.TotalBytes = out.MembershipsBytes + out.HistoryBytes + out.StepsBytes + out.EvidenceBytes
	out.TotalMB = math.Round(float64(out.TotalBytes)/1048576*100) / 100
	return out
}

// GetRegionsSnapshotMemory sizes the retained Regions snapshot, or returns nil
// before the first background relay snapshot is published. It takes only
// repeaterEnrichMu, and releases it before walking the immutable snapshot, so
// it never blocks ingest or eviction.
func (s *PacketStore) GetRegionsSnapshotMemory() *RegionsSnapshotMemory {
	s.repeaterEnrichMu.Lock()
	snapshot := s.repeaterRelaySnapshot
	s.repeaterEnrichMu.Unlock()
	if snapshot == nil || snapshot.routes == nil {
		return nil
	}
	return measureRegionsSnapshot(snapshot)
}
