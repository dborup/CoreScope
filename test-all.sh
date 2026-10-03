#!/bin/sh
# Run every unit test file, then print a summary.
#
# A failing file does not stop the run: every file runs, the failures are
# listed at the end, and the script exits 1 if any file failed.
# Only files that run with plain `node` and no server belong here. E2E files
# run in the Playwright step of .github/workflows/deploy.yml.

passed=0
failed=0
failed_files=

run() {
  echo ""
  echo "── $1"
  if node "$1"; then
    passed=$((passed + 1))
  else
    failed=$((failed + 1))
    failed_files="$failed_files $1"
  fi
}

echo "═══════════════════════════════════════"
echo "  CoreScope — Test Suite"
echo "═══════════════════════════════════════"

# Unit tests (deterministic, fast)
echo ""
echo "── Unit Tests ──"
run test-packet-filter.js
run test-packet-filter-ux.js
run test-issue-121-clear-filters-selection.js
run test-clear-filters.js
run test-aging.js
run test-issue-1065-gesture-hints-gates.js
run test-frontend-helpers.js
run test-app-api-inflight-cleanup-rejection.js
run test-issue-120-distance-building.js
run test-privacy-page.js
run test-nav-dynamic-link-lifecycle.js
run test-nav-first-load-fit.js
run test-nav-priority-scheduler.js
run test-geo-filter.js
run test-fetch-all-nodes-pagination.js
run test-nodes-geo-scope-filter.js
run test-nodes-export.js
run test-nodes-export-wiring.js
run test-nodes-export-order.js
run test-nodes-export-narrow-wait.js
run test-my-repeaters-dashboard.js
run test-repeater-metric-scatter.js
run test-top-routes-overlay.js
run test-important-links-byte-filter.js
run test-url-state.js
run test-node-adverts.js
run test-perf-go-runtime.js
run test-channel-psk-ux.js
run test-channel-sidebar-layout.js
run test-channel-fluid-layout.js
run test-channel-modal-ux.js
run test-channel-decrypt-insecure-context.js
run test-channel-qr.js
run test-channel-qr-wiring.js
run test-channel-issue-1087.js
run test-issue-1409-no-encrypted-flood.js
run test-analytics-channels-integration.js
run test-analytics-foreign-traffic-tab.js
run test-analytics-nodes-without-scope.js
run test-analytics-table-ids-unique.js
run test-analytics-relay-airtime-dumbbell.js
run test-live-vcr-mode-contrast.js
run test-observers-headings.js
run test-issue-1789-observer-firmware-cols.js
run test-issue-1648-m1-emoji-scan.js
run test-issue-1648-m2-emoji-scan.js
run test-issue-1648-m3-emoji-scan.js
run test-issue-1648-m6-final-sweep.js
run test-issue-1648-m6-lint-self.js
run test-issue-1883-redirect-history.js
run test-issue-1890-og-url.js
run test-traces.js
run test-live-multibyte-filter.js
run test-issue-124-rx-coverage-viewport.js
run test-rx-coverage-escape.js
run test-issue-125-live-toggles-wiring.js
run test-issue-2052-touch-target-css.js
run test-channel-proposals.js
run test-packet-detail-channel-xss.js
run test-channels-name-escaping.js

# #1418 — route-view v2 (Tufte) coverage
run test-issue-1418-raw-hex-extraction.js
run test-issue-1418-edge-weights.js
run test-issue-1418-cb-preset-ramp.js
run test-issue-1418-spider-fan.js
run test-issue-1418-deeplink-hops-channels.js
run test-issue-1418-polish-review.js
run test-issue-1420-tile-providers.js
run test-issue-1614-tile-url-function.js
run test-issue-1438-marker-css-vars.js
run test-issue-1438-customizer-mcrole.js
run test-issue-1446-cb-preset-cascade.js
run test-issue-1380-cb-sim-overlay.js
run test-issue-1380-cb-reset-button.js
run test-issue-1450-logo-aspect.js
run test-issue-1454-channels-toggle.js
run test-issue-1456-score-labels.js

# #1461 mobile UX overhaul + #1470 node-detail tile helper (#1468 covered by E2E)
run test-issue-1461-mobile-page-actions.js
run test-issue-1470-node-tile-helper.js
run test-issue-1485-live-anim-z.js
run test-live-anims.js
run test-issue-1532-live-fullscreen.js
run test-issue-1833-legend-toggle-vcr-offset.js
run test-naive-banner-tone.js
run test-issue-1473-reserved-prefixes.js
run test-issue-1473-prefix-generator.js
run test-issue-1770-mobile-row-clamp.js
run test-issue-1849-trace-hashbytes.js
run test-node-analytics-hop-chart.js
run test-analytics-hop-depth-ui.js
run test-channels-ping-bot-reply.js
run test-channels-observed-path-hash-size.js
run test-packet-path-map.js
run test-area-nodes-map.js
run test-ping-scores.js
run test-observer-neighbors-report-badge.js
run test-observer-direct-neighbors-panel.js
run test-analytics-areas-tab.js
run test-observer-neighbors-tool.js
run test-new-nodes-tool.js
run test-node-changes-tool.js
run test-network-digest-tool.js
run test-position-gaps-tool.js
run test-gps-sanity-tool.js
run test-map-scope-filter.js
run test-issue-117-ws-watchdog.js
run test-issue-111-drawer-version.js

# test-all.sh self-test (#174)
run test-test-all.js

echo ""
echo "═══════════════════════════════════════"
echo "  $passed passed, $failed failed ($((passed + failed)) files)"
if [ "$failed" -gt 0 ]; then
  echo "  Failed:"
  for f in $failed_files; do
    echo "    $f"
  done
  echo "═══════════════════════════════════════"
  exit 1
fi
echo "  All tests passed"
echo "═══════════════════════════════════════"
