package main

import (
	"strings"
	"testing"

	"github.com/meshcore-analyzer/buildings"
	"github.com/meshcore-analyzer/dhm"
)

// #730 M3 — the dhm block of /api/config/client carries the footprints'
// credit parts, so the profile panel builds the credit of a classified
// profile from the server (internal/buildings) and keeps no copy of it.
// Present only while the profile is served with the classification on.
func TestClientConfigBuildingsCredit(t *testing.T) {
	up := wcsUpstream(t, tiffAnswer)
	want := DHMBuildingsClientConfig{
		Credit:      buildings.AttributionDerived,
		DatasetName: buildings.DatasetName,
		DatasetURL:  buildings.DatasetURL,
		SourceName:  buildings.Source,
		SourceURL:   buildings.SourceTermsURL,
	}
	for _, tc := range []struct {
		name      string
		profile   bool
		buildings bool
		want      bool
	}{
		{"buildings on", true, true, true},
		{"buildings off", true, false, false},
		{"profile off", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			srv.cfg.DHM = DHMConfig{Enabled: true, Token: dhmTestToken}
			srv.dhm = newTestDHMProxy(t, up, &dhmLogSink{}, nil)
			if tc.profile {
				pc := dhm.ProfileConfig{Enabled: true, Buildings: dhm.BuildingsConfig{Enabled: tc.buildings}}
				srv.terrain = newTerrainProfiler(pc, nil, terrainProfilerDeps{
					src: newFakeElevation(t, flatElev(10)), logf: (&dhmLogSink{}).logf,
					buildings: newStubOGC(t).source(terrainSettings{maxInFlight: 1}),
				})
			}
			block := clientDHMBlock(t, router)
			if block == nil {
				t.Fatal("no dhm block while the proxy is active")
			}
			if (block.Buildings != nil) != tc.want {
				t.Fatalf("dhm.buildings = %+v, want present: %v", block.Buildings, tc.want)
			}
			if tc.want && *block.Buildings != want {
				t.Fatalf("dhm.buildings = %+v, want %+v", *block.Buildings, want)
			}
		})
	}
}

// TestClientConfigBuildingsCreditMarks: the page's credit walker turns
// each mark into a link in text order (public/map-tile-providers.js
// creditLinks), so the classified credit must hold the DHM dataset, the
// footprints' dataset, their source and the licence once each, in that
// order, and every target must be https. The @geodanmark handle is text,
// never a link: it is no mark.
func TestClientConfigBuildingsCreditMarks(t *testing.T) {
	d := dhmClientConfig()
	b := buildingsClientConfig()
	marks := []struct{ mark, url string }{
		{d.DatasetName, d.DatasetURL},
		{b.DatasetName, b.DatasetURL},
		{b.SourceName, b.SourceURL},
		{d.LicenseName, d.LicenseURL},
	}
	rest := b.Credit
	for _, m := range marks {
		if strings.Count(b.Credit, m.mark) != 1 {
			t.Fatalf("mark %q occurs %d times in %q", m.mark, strings.Count(b.Credit, m.mark), b.Credit)
		}
		i := strings.Index(rest, m.mark)
		if i < 0 {
			t.Fatalf("mark %q is not after the previous one in %q", m.mark, b.Credit)
		}
		rest = rest[i+len(m.mark):]
		if !strings.HasPrefix(m.url, "https://") {
			t.Fatalf("mark %q links %q", m.mark, m.url)
		}
	}
	if !strings.Contains(b.Credit, buildings.SourceHandle) || b.SourceName == buildings.SourceHandle {
		t.Fatalf("credit %q, source mark %q: the handle must be text", b.Credit, b.SourceName)
	}
}
