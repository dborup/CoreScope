package main

import (
	"testing"

	"github.com/meshcore-analyzer/copdem"
	"github.com/meshcore-analyzer/dhm"
)

// #738 M2 — the dhm block of /api/config/client carries the foreign
// fill's credit parts, so the profile panel builds the Copernicus credit
// from the server (copdem) and keeps no copy of it. Present only when the
// profile is served with the fill on: without it no profile is mixed.
func TestClientConfigFillCredit(t *testing.T) {
	up := wcsUpstream(t, tiffAnswer)
	want := DHMFillClientConfig{
		Credit:      copdem.Credit,
		DatasetName: copdem.DatasetName,
		DatasetURL:  copdem.DatasetURL,
		LicenseName: copdem.LicenseName,
		LicenseURL:  copdem.LicenseURL,
	}
	for _, tc := range []struct {
		name    string
		profile bool
		fill    bool
		want    bool
	}{
		{"fill on", true, true, true},
		{"fill off", true, false, false},
		{"profile off", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, router := setupTestServer(t)
			srv.cfg.DHM = DHMConfig{Enabled: true, Token: dhmTestToken}
			srv.dhm = newTestDHMProxy(t, up, &dhmLogSink{}, nil)
			if tc.profile {
				pc := dhm.ProfileConfig{Enabled: true, ForeignFill: dhm.ForeignFillConfig{Enabled: tc.fill}}
				srv.terrain = newTerrainProfiler(pc, nil, terrainProfilerDeps{
					src: newFakeElevation(t, flatElev(10)), logf: (&dhmLogSink{}).logf,
					fill: &scriptedFill{fn: everywhere(1)},
				})
			}
			block := clientDHMBlock(t, router)
			if block == nil {
				t.Fatal("no dhm block while the proxy is active")
			}
			if (block.Fill != nil) != tc.want {
				t.Fatalf("dhm.fill = %+v, want present: %v", block.Fill, tc.want)
			}
			if tc.want && *block.Fill != want {
				t.Fatalf("dhm.fill = %+v, want %+v", *block.Fill, want)
			}
		})
	}
}
