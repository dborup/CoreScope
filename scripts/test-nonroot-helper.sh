#!/usr/bin/env bash
# scripts/test-nonroot-helper.sh — unit tests for docker/nonroot.sh (#339).
#
# No Docker and no root: exercises the helper's pure parts (supervisor config
# rendering, setting/ID validation, the writable-path list) and the docs.
# The ownership migration itself needs root and is covered by
# scripts/test-docker-nonroot.sh against a built image.
#
# Run: bash scripts/test-nonroot-helper.sh
# SHELL_UNDER_TEST=busybox-sh-like command to run the helper with (default sh).
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HELPER="$ROOT/docker/nonroot.sh"
read -r -a RUN <<< "${SHELL_UNDER_TEST:-sh}"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); }
bad() { FAIL=$((FAIL + 1)); echo "FAIL: $*" >&2; }
assert_eq() { if [ "$2" = "$3" ]; then ok; else bad "$1 — expected '$2' got '$3'"; fi; }

helper() { "${RUN[@]}" "$HELPER" "$@"; }

# ----- render-conf ----------------------------------------------------------
# The generated config must be the shipped one plus exactly one `user=` line
# directly under each Go program, so Mosquitto and Caddy keep running exactly
# as configured today.
for conf in "$ROOT"/docker/supervisord-go*.conf; do
    name=$(basename "$conf")
    if ! out=$(helper render-conf "$conf" corescope 2> "$TMP/err"); then
        bad "$name: render-conf failed: $(cat "$TMP/err")"
        continue
    fi
    printf '%s\n' "$out" > "$TMP/out"
    assert_eq "$name: base has no user=corescope line" "0" "$(grep -cx 'user=corescope' "$conf")"
    assert_eq "$name: two user= lines added" "2" "$(grep -cx 'user=corescope' "$TMP/out")"
    if grep -vx 'user=corescope' "$TMP/out" | cmp -s - "$conf"; then ok; else bad "$name: anything besides the user= lines changed"; fi
    for prog in corescope-ingestor corescope-server; do
        after=$(grep -A1 -x "\[program:$prog\]" "$TMP/out" | sed -n 2p)
        assert_eq "$name: $prog runs as corescope" "user=corescope" "$after"
    done
    for prog in mosquitto caddy; do
        grep -qx "\[program:$prog\]" "$conf" || continue
        section=$(awk -v h="[program:$prog]" '$0==h{on=1;next} /^\[/{on=0} on' "$TMP/out")
        if printf '%s\n' "$section" | grep -q '^user='; then bad "$name: $prog got a user= line"; else ok; fi
    done
done

# A config without both Go programs must not silently leave one as root.
grep -v -x '\[program:corescope-server\]' "$ROOT/docker/supervisord-go.conf" > "$TMP/broken.conf"
if helper render-conf "$TMP/broken.conf" corescope > /dev/null 2>&1; then
    bad "render-conf accepted a config without [program:corescope-server]"
else ok; fi

# ----- validate -------------------------------------------------------------
# Accounts of the Alpine runtime image (abridged): caddy uid 100 / gid 101,
# mosquitto uid 101 / gid 102, users gid 100, shadow gid 42.
cat > "$TMP/passwd" << 'EOF'
root:x:0:0:root:/root:/bin/sh
ntp:x:123:123:NTP:/var/empty:/sbin/nologin
guest:x:405:100:guest:/dev/null:/sbin/nologin
nobody:x:65534:65534:nobody:/:/sbin/nologin
caddy:x:100:101:caddy:/var/lib/caddy:/sbin/nologin
mosquitto:x:101:102:mosquitto:/var/empty:/sbin/nologin
corescope:x:10001:10001:CoreScope:/nonexistent:/sbin/nologin
EOF
cat > "$TMP/group" << 'EOF'
root:x:0:root
wheel:x:10:root
shadow:x:42:
www-data:x:82:caddy
users:x:100:games
caddy:x:101:caddy
mosquitto:x:102:mosquitto
nogroup:x:65533:
EOF

# validate_case LABEL EXPECT(ok|fail) SETTING UID GID [stdout]
validate_case() {
    local label=$1 expect=$2 setting=$3 uid=$4 gid=$5 want_out=${6-}
    local out rc
    out=$(env -u CORESCOPE_RUN_AS_NONROOT -u CORESCOPE_UID -u CORESCOPE_GID \
        NONROOT_PASSWD="$TMP/passwd" NONROOT_GROUP="$TMP/group" \
        ${setting:+CORESCOPE_RUN_AS_NONROOT="$setting"} \
        ${uid:+CORESCOPE_UID="$uid"} ${gid:+CORESCOPE_GID="$gid"} \
        "${RUN[@]}" "$HELPER" validate 2> "$TMP/verr")
    rc=$?
    if [ "$expect" = ok ]; then
        if [ "$rc" -eq 0 ]; then ok; else bad "validate $label: rc=$rc ($(cat "$TMP/verr"))"; fi
        [ -z "$want_out" ] || assert_eq "validate $label: output" "$want_out" "$out"
    else
        if [ "$rc" -ne 0 ] && grep -q 'ERROR' "$TMP/verr"; then ok; else bad "validate $label: expected a clear failure, rc=$rc"; fi
    fi
}
validate_case "1 with ids" ok 1 1000 1000 "1000:1000"
validate_case "true with ids" ok true 1000 1000 "1000:1000"
validate_case "existing corescope entry" ok 1 10001 10001 "10001:10001"
validate_case "gid of the users group" ok 1 99 100 "99:100"
validate_case "yes is not a value" fail yes 1000 1000
validate_case "TRUE is not a value" fail TRUE 1000 1000
validate_case "missing uid" fail 1 "" 1000
validate_case "missing gid" fail 1 1000 ""
validate_case "uid 0" fail 1 0 1000
validate_case "gid 0" fail 1 1000 0
validate_case "leading zero" fail 1 01000 1000
validate_case "non-numeric uid" fail 1 abc 1000
validate_case "negative uid" fail 1 -5 1000
validate_case "uid too large" fail 1 2147483648 1000
validate_case "uid of mosquitto" fail 1 101 1000
validate_case "uid of caddy" fail 1 100 1000
validate_case "uid of nobody" fail 1 65534 1000
validate_case "gid of mosquitto" fail 1 1000 102
validate_case "gid of caddy" fail 1 1000 101
validate_case "system gid (shadow)" fail 1 1000 42

# ----- writable paths -------------------------------------------------------
# Every file or directory the Go services create next to the database must be
# in the migration list, or a non-root service hits a root-owned leftover.
paths=$(helper paths)
go_const() { sed -En "s/^[[:space:]]*(const[[:space:]]+)?$2[[:space:]]*=[[:space:]]*\"([^\"]*)\".*/\2/p" "$ROOT/$1" | head -1; }
expect_paths="meshcore.db meshcore.db-wal meshcore.db-shm
ping_scores_history.db ping_scores_history.db-wal ping_scores_history.db-shm"
for spec in cmd/ingestor/client_rx_delete.go:clientRxSaltFileName \
    cmd/ingestor/client_rx_delete.go:clientRxAuditFileName \
    cmd/ingestor/client_rx_delete_queue.go:clientRxQueueDirName \
    internal/prunequeue/prunequeue.go:QueueDirName \
    internal/mbcapqueue/mbcapqueue.go:QueueDirName \
    internal/channelregistry/queue.go:QueueDirName; do
    v=$(go_const "${spec%%:*}" "${spec#*:}")
    if [ -n "$v" ]; then ok; else bad "could not read ${spec#*:} from ${spec%%:*}"; fi
    expect_paths="$expect_paths $v"
done
assert_eq "ping score history DB name in Go" "ping_scores_history.db" \
    "$(grep -o '"ping_scores_history.db"' "$ROOT/cmd/server/ping_score_history.go" | head -1 | tr -d '"')"
for p in $expect_paths; do
    if printf '%s\n' "$paths" | grep -qx "[fd] $p"; then ok; else bad "writable path $p is not in the migration list"; fi
done
assert_eq "migration list is exactly the known paths" \
    "$(printf '%s' "$expect_paths" | tr ' ' '\n' | sort)" "$(printf '%s\n' "$paths" | cut -d' ' -f2 | sort)"

# ----- image wiring and docs ------------------------------------------------
for df in Dockerfile Dockerfile.go; do
    if grep -qx 'COPY docker/nonroot.sh /usr/local/lib/corescope/nonroot.sh' "$ROOT/$df"; then ok; else bad "$df does not install docker/nonroot.sh"; fi
done
for doc in docker-compose.example.yml docs/deployment.md; do
    for v in CORESCOPE_RUN_AS_NONROOT CORESCOPE_UID CORESCOPE_GID; do
        if grep -q "$v" "$ROOT/$doc"; then ok; else bad "$doc does not document $v"; fi
    done
done
section=$(awk '/^### Running the Go services without root/{on=1;print;next} on&&/^##[^#]/{on=0} on&&/^### /&&!/without root/{on=0} on' "$ROOT/docs/deployment.md")
for want in 'scripts/backup-sqlite.sh' 'Rollback' 'set-owner' 'host'; do
    if printf '%s\n' "$section" | grep -q -- "$want"; then ok; else bad "deployment.md non-root section lacks '$want'"; fi
done

echo "test-nonroot-helper: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
