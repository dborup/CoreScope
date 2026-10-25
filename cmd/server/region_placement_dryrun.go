package main

import (
	"context"
	"io"
)

// STUB (red commit): the operator dry-run report for #488 M2.

var regionDryRunSweep = []int{0, 1, 2, 3, 4, 5, 7}

type regionBasisCounts struct {
	Direct, Fewest, Unknown, Transient int
	FewestByHops                       [regionPlacementHopSlots]int
}

type regionDryRunRegion struct {
	Region                                            string
	Heard, Placed, Direct, Fewest, Removed, Transient int
}

type regionDryRunSweepRow struct {
	MaxHops, Direct, Fewest, Unknown int
}

type regionPlacementDryRun struct {
	Settings  regionPlacementSettings
	Watermark int64
	ObsRawHex bool
	Nodes     int
	Pairs     int
	Hearings  regionHearingCounts
	Bases     regionBasisCounts
	Regions   []regionDryRunRegion
	Sweep     []regionDryRunSweepRow
}

func buildRegionPlacementDryRun(ctx context.Context, db *DB, s regionPlacementSettings, sweep []int) (*regionPlacementDryRun, error) {
	return &regionPlacementDryRun{}, nil
}

func runRegionPlacementDryRun(dbPath string, s regionPlacementSettings, w io.Writer) error {
	return nil
}

func (r *regionPlacementDryRun) write(w io.Writer) error {
	return nil
}
