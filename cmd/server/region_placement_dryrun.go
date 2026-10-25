package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Read-only dry-run of the #488 placement rule for operators
// (`corescope-server -region-placement-dryrun`). It builds the same global
// aggregate the placed mode serves from, on the database the server would
// open, and prints counts only — per evidence kind, per basis, per region,
// and the unplaced share for several maxHops values — so the maxHops
// default can be chosen from a real instance before placed mode is turned
// on. It names no node: no public key, no name, no observer.

// regionDryRunSweep are the maxHops values the report compares.
var regionDryRunSweep = []int{0, 1, 2, 3, 4, 5, 6, 7}

// regionBasisCounts counts nodes per placement basis.
type regionBasisCounts struct {
	Direct, Fewest, Unknown int
	// Transient counts nodes with at least one transient region.
	Transient int
	// FewestByHops counts fewest_hops nodes by the hop count that placed
	// them (0 = a legacy '[]').
	FewestByHops [regionPlacementHopSlots]int
}

func (c *regionBasisCounts) add(p regionPlacement) {
	switch p.Basis {
	case placementBasisDirect:
		c.Direct++
	case placementBasisFewest:
		c.Fewest++
		c.FewestByHops[p.Hops]++
	default:
		c.Unknown++
	}
	if len(p.Transient) > 0 {
		c.Transient++
	}
}

// regionDryRunRegion is one region under the configured settings.
type regionDryRunRegion struct {
	Region string
	// Heard is what today's filter returns for the region.
	Heard int
	// Placed = Direct + Fewest nodes placed in the region.
	Placed, Direct, Fewest int
	// Removed = Heard - Placed: heard here, placed elsewhere or nowhere.
	Removed int
	// Transient counts nodes for which this region is transient.
	Transient int
}

// regionDryRunSweepRow is the basis split at one maxHops value.
type regionDryRunSweepRow struct {
	MaxHops, Direct, Fewest, Unknown int
}

// regionPlacementDryRun is the whole report.
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
	Elapsed   time.Duration
}

// buildRegionPlacementDryRun scans the full history once and evaluates the
// rule under s and under each maxHops in sweep.
func buildRegionPlacementDryRun(ctx context.Context, db *DB, s regionPlacementSettings, sweep []int) (*regionPlacementDryRun, error) {
	start := time.Now()
	wm, err := db.maxObservationID(ctx)
	if err != nil {
		return nil, err
	}
	agg := newRegionEvidenceAgg()
	if _, err := db.scanRegionEvidence(ctx, 0, wm, false, agg); err != nil {
		return nil, err
	}
	r := &regionPlacementDryRun{
		Settings: s, Watermark: wm, ObsRawHex: db.hasObsRawHex(),
		Nodes: len(agg.nodes), Pairs: agg.pairs, Hearings: agg.hearings,
	}
	rows := map[string]*regionDryRunRegion{}
	row := func(region string) *regionDryRunRegion {
		if rows[region] == nil {
			rows[region] = &regionDryRunRegion{Region: region}
		}
		return rows[region]
	}
	for _, per := range agg.nodes {
		for region := range per {
			row(region).Heard++
		}
		p := placeNode(per, s.Params)
		r.Bases.add(p)
		for _, region := range p.Regions {
			x := row(region)
			x.Placed++
			if p.Basis == placementBasisDirect {
				x.Direct++
			} else {
				x.Fewest++
			}
		}
		for _, region := range p.Transient {
			row(region).Transient++
		}
	}
	for _, x := range rows {
		x.Removed = x.Heard - x.Placed
		r.Regions = append(r.Regions, *x)
	}
	sort.Slice(r.Regions, func(i, j int) bool { return r.Regions[i].Region < r.Regions[j].Region })
	for _, h := range sweep {
		p := s.Params
		p.MaxHops = h
		var c regionBasisCounts
		for _, per := range agg.nodes {
			c.add(placeNode(per, p))
		}
		r.Sweep = append(r.Sweep, regionDryRunSweepRow{MaxHops: h, Direct: c.Direct, Fewest: c.Fewest, Unknown: c.Unknown})
	}
	r.Elapsed = time.Since(start)
	return r, nil
}

func dryRunShare(n, of int) string {
	if of == 0 {
		return "–"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(of))
}

// write prints the report as Markdown, ready to paste into an issue.
func (r *regionPlacementDryRun) write(w io.Writer) error {
	b := &strings.Builder{}
	p := r.Settings.Params
	lifetime := fmt.Sprintf("lifetimeDays %d", int(p.Lifetime/(24*time.Hour)))
	if p.Lifetime == 0 {
		lifetime += " (no window)"
	}
	span := fmt.Sprintf("persistSpanHours %d", int(p.PersistSpan/time.Hour))
	if p.PersistSpan == 0 {
		span += " (transient rule off)"
	}
	raw := "no (every hearing is classified by path_json; a legacy '[]' is fallback only)"
	if r.ObsRawHex {
		raw = "yes"
	}
	fmt.Fprintf(b, "# Region placement dry-run (#488)\n\n")
	fmt.Fprintf(b, "Read-only, counts only: no node is named. Placement is inferred from reception, not a verified location.\n\n")
	fmt.Fprintf(b, "- Settings: mode %s (the filter in use), maxHops %d, %s, %s\n", r.Settings.Mode, p.MaxHops, lifetime, span)
	fmt.Fprintf(b, "- Observations scanned: ids up to %d; per-observation frames (observations.raw_hex): %s\n", r.Watermark, raw)
	fmt.Fprintf(b, "- Nodes with advert evidence in a region: %d; (node, region) pairs: %d; built in %v\n\n",
		r.Nodes, r.Pairs, r.Elapsed.Round(time.Millisecond))

	fmt.Fprintf(b, "## Advert observations by evidence\n\n| Evidence | Observations |\n|---|---:|\n")
	for _, e := range []struct {
		label string
		kind  regionHearingKind
	}{
		{"direct: own transmission, hash count 0 on the observation's frame", hearingDirect},
		{"legacy '[]' without a frame (fallback only)", hearingLegacy},
		{fmt.Sprintf("relayed 1-%d hops", regionPlacementHopSlots-1), hearingRelayed},
		{fmt.Sprintf("relayed %d+ hops (never places)", regionPlacementHopSlots), hearingFar},
		{"contact re-share (no evidence)", hearingShared},
		{"unknown path or undated (no evidence)", hearingUnknown},
	} {
		fmt.Fprintf(b, "| %s | %d |\n", e.label, r.Hearings[e.kind])
	}

	fmt.Fprintf(b, "\n## Nodes by basis (maxHops %d)\n\n| Basis | Nodes | Share |\n|---|---:|---:|\n", p.MaxHops)
	fmt.Fprintf(b, "| direct | %d | %s |\n", r.Bases.Direct, dryRunShare(r.Bases.Direct, r.Nodes))
	for h, n := range r.Bases.FewestByHops {
		if n > 0 {
			fmt.Fprintf(b, "| fewest_hops, hops = %d | %d | %s |\n", h, n, dryRunShare(n, r.Nodes))
		}
	}
	fmt.Fprintf(b, "| unknown (not placed) | %d | %s |\n", r.Bases.Unknown, dryRunShare(r.Bases.Unknown, r.Nodes))
	if p.PersistSpan > 0 {
		fmt.Fprintf(b, "| with a transient region | %d | %s |\n", r.Bases.Transient, dryRunShare(r.Bases.Transient, r.Nodes))
	}

	fmt.Fprintf(b, "\n## Per region (maxHops %d)\n\n", p.MaxHops)
	fmt.Fprintf(b, "| Region | Heard (today's filter) | Placed | Direct | Fewest hops | Heard, not placed here | Transient |\n|---|---:|---:|---:|---:|---:|---:|\n")
	for _, x := range r.Regions {
		fmt.Fprintf(b, "| %s | %d | %d | %d | %d | %d | %d |\n", x.Region, x.Heard, x.Placed, x.Direct, x.Fewest, x.Removed, x.Transient)
	}

	fmt.Fprintf(b, "\n## Unknown share by maxHops\n\n| maxHops | Direct | Fewest hops | Unknown | Unknown share |\n|---:|---:|---:|---:|---:|\n")
	for _, x := range r.Sweep {
		fmt.Fprintf(b, "| %d | %d | %d | %d | %s |\n", x.MaxHops, x.Direct, x.Fewest, x.Unknown, dryRunShare(x.Unknown, r.Nodes))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// runRegionPlacementDryRun opens dbPath read-only (OpenDB, mode=ro), writes
// the report to w and closes the database.
func runRegionPlacementDryRun(dbPath string, s regionPlacementSettings, w io.Writer) error {
	db, err := OpenDB(dbPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer db.Close()
	r, err := buildRegionPlacementDryRun(context.Background(), db, s, regionDryRunSweep)
	if err != nil {
		return err
	}
	return r.write(w)
}
