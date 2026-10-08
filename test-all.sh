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
run test-issue-147-packets-url-detail-params.js
run test-issue-96-hide-control.js
run test-issue-180-packets-detail-close.js
run test-aging.js
run test-issue-1065-gesture-hints-gates.js
run test-frontend-helpers.js
run test-app-api-inflight-cleanup-rejection.js
run test-app-api-bust-inflight-243.js
run test-nodes-advert-ws-bust-279.js
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
run test-issue-254-affinity-debug-toggle.js
run test-issue-258-column-widths.js
run test-issue-259-nodes-esc-listener.js
run test-issue-282-pktesc-listener.js
run test-issue-282-comment-guards.js
run test-issue-314-chscroll-listener.js
run test-issue-322-comment-guards.js
run test-backup-sqlite-safety.js
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
run test-issue-165-hop-ambiguity-badge.js
run test-issue-165-hop-resolution-per-observer.js
run test-issue-1418-polish-review.js
run test-issue-1420-tile-providers.js
run test-issue-1614-tile-url-function.js
run test-issue-332-tile-provider-fallback.js
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
run test-channels-client-state-152.js
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
run test-estimated-positions-config.js
run test-map-scope-filter.js
run test-issue-117-ws-watchdog.js
run test-issue-111-drawer-version.js

# Previously run only by the JS unit step in deploy.yml (#174)
run test-packet-filter-time.js
run test-packets.js
run test-confidence-indicator.js
run test-1659-analytics-warmup.js
run test-analytics-tab-state-and-query.js
run test-analytics-subtab-deeplinks-205.js
run test-hash-stats-sort-226.js
run test-analytics-sorting-236.js
run test-channels-merge-1498-unit.js
run test-issue-1518-home-url.js
run test-live-region-filter.js
run test-issue-1136-observer-iata-map.js
run test-channel-issue-1101.js
run test-observer-iata-1188.js
run test-pull-to-reconnect-1091.js
run test-issue-1279-p2-code-filter.js
run test-area-filter.js
run test-issue-1293-marker-shapes.js
run test-issue-1356-map-a11y.js
run test-issue-1360-pill-letter-count.js
run test-issue-1364-pill-no-clamp.js
run test-issue-1375-scope-stats-fetch.js
run test-issue-1361-cb-presets.js
run test-issue-1407-cb-preset-propagation.js
run test-issue-1412-customizer-no-override.js
run test-issue-1846-observers-width.js
run test-issue-1562-observers-summary.js
run test-issue-1509-nav-active-bg.js
run test-issue-1509-detect-preset.js
run test-live.js
run test-issue-220-live-filter-readiness.js
run test-coverage-gate.js
run test-node-reach-coverage.js
run test-reach-rank.js
run test-issue-1107-live-layout.js
run test-issue-1619-feed-detail-card-draggable.js
run test-xss-escape-sinks.js
run test-preflight-xss-gate.js
run test-issue-1648-m4-emoji-scan.js
run test-issue-1753-copy-url-slash.js
run test-issue-1668-m3-typography.js
run test-mqtt-status-panel.js
run test-issue-1697-mqtt-mobile-e2e.js
run test-warmup-banner.js
run test-issue-1633-hide-1byte-hops.js
run test-issue-1668-m4-per-route.js
run test-a11y-axe-1668-selftest.js
run test-a11y-1716-rf-range-btn-active.js
run test-issue-1705-subpath-contrast.js
run test-a11y-axe-routes-coverage.js

# Green unit tests that no runner registered before (#174 inventory)
run test-1108-region-hide-nodes.js
run test-a11y-1715-dark-role-swatches.js
run test-analytics-distance-view-path.js
run test-analytics-wardriving-tab.js
run test-anl1-tooltip-render.js
run test-channel-color-picker.js
run test-channel-decrypt-ecb.js
run test-channel-decrypt-m345.js
run test-channel-live-decrypt-userprefix.js
run test-channel-live-decrypt.js
run test-color-picker-ux.js
run test-compare-flood-filter.js
run test-compare-overlap.js
run test-embed-mode-1369.js
run test-geofilter-draft.js
run test-hash-color.js
run test-issue-1166-first-seen-column.js
run test-issue-199-missing-node.js
run test-issue-1189-composed-cell.js
run test-issue-1189-live-iata-badge.js
run test-issue-1415-packets-layout.js
run test-issue-1488-marker-stroke-vars.js
run test-issue-1496-reset-all-complete.js
run test-issue-1563-aggregate-and-inflight.js
run test-issue-1574-live-map-max-nodes.js
run test-issue-1606-pagination.js
run test-issue-1644-redesign.js
run test-issue-1648-followup-phosphor-leaks.js
run test-issue-1648-m5-emoji-scan.js
run test-issue-1668-m2-contrast.js
run test-issue-1825-observer-node-cross-links.js
run test-issue-1836-crossnav-case-normalization.js
run test-issue-1843-node-qr-quiet-zone.js
run test-live-dt-cap-1524.js
run test-live-legend-helper.js
run test-node-reach-coverage-debounce.js
run test-observer-detail-view-path.js
run test-observer-naive-clock-1478.js
run test-path-inspector.js
run test-payload-labels-content.js
run test-payload-labels-namespace.js
run test-perf-anomaly.js
run test-perf-render-1258.js
run test-pull-to-reconnect.js
run test-rx-coverage-config-race.js
run test-slideover-1056-rowsel-strict.js
run test-map-clustering.js
run test-panel-corner.js
run test-customizer-v2.js

# Repaired orphans (#189)
run test-channel-colors.js
run test-channel-ux-followup.js
run test-channel-ux-round2.js
run test-drag-manager.js
run test-fluid-scaffolding.js
run test-hop-resolver-affinity.js
run test-issue-1470-card-bg-contrast.js
run test-issue-1646-compare-polish.js
run test-perf-disk-io-1120.js
run test-table-sort.js

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
