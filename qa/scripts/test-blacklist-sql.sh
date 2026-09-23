#!/usr/bin/env bash
# test-blacklist-sql.sh — unit tests for the §10.2 SQL construction in
# qa/scripts/blacklist-test.sh (issue #1977). Sources the script and exercises
# its pure helpers, plus a real local sqlite3 against throwaway fixture DBs.
#
# Run: bash qa/scripts/test-blacklist-sql.sh
# Exits non-zero if any case fails.
#
# The point of the sqlite3 group is that BOTH directions are asserted. A test
# that only checks "the injection payload returns 0" passes just as happily when
# the query is silently broken and returns 0 for everything, so the legitimate
# pubkey must be shown to still return the row it should.
#
# The fixture schema is NOT hand-written: it is the transmissions CREATE TABLE
# extracted from cmd/ingestor/db.go, and the query is also run against the
# committed staging-captured test-fixtures/e2e-fixture.db. An earlier version
# invented a `from_node` column that no CoreScope database has, and so passed
# while the real probe could never succeed.
#
# The fake-target group (issue #83) runs the whole script, unmodified, against
# fake ssh/docker/curl with every other command behind an argv-logging shim,
# and checks the runner contract end to end: no pubkey, SQL, URL or token in
# any process's argv; each sqlite runner reads only its own db path, and a
# wrong path is refused without creating a file; removed variables refuse to
# start; and every success, failure, signal and broken-stream run restores
# the config, leaves the databases and filesystem as they were, and exits with
# its classified status. It needs sqlite3, jq and (for the broken-stream
# cases) perl.

# ShellCheck (run with -x so the sourced script's globals are known):
#   SC2016 — single-quoted $… is literal on purpose: injection payloads, and
#            code for a child bash/perl to expand.
#   SC2030/SC2031 — the fake-target runs set PATH and FAKE_* inside ( … ) on
#            purpose, so nothing leaks into the next run.
# shellcheck disable=SC2016,SC2030,SC2031
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
INGESTOR_DB_GO="$REPO_ROOT/cmd/ingestor/db.go"
REAL_FIXTURE="$REPO_ROOT/test-fixtures/e2e-fixture.db"
# BLACKLIST_TEST_SH points the suite at another copy of the script (used to
# check that deliberately broken variants fail it).
BLACKLIST_SH="${BLACKLIST_TEST_SH:-$SCRIPT_DIR/blacklist-test.sh}"
# shellcheck source=blacklist-test.sh
. "$BLACKLIST_SH"

# Stream breaker: `perl -e "$PERL_BREAK" FD cmd...` runs cmd with FD (1 or 2)
# on a pipe whose reader is already closed, so the first write to it raises
# SIGPIPE — deterministically, unlike a pipe into a process that exits "soon".
# SIGPIPE is reset to default first, in case this suite itself was started with
# it ignored. `perl -e "$PERL_IGNORE" FD cmd...` does the same but starts cmd
# with SIGPIPE ignored, which bash cannot trap.
PERL_BREAK='my $fd = shift; $SIG{PIPE} = "DEFAULT"; pipe(my $r, my $w) or die "pipe: $!"; close $r;
  if ($fd == 1) { open(STDOUT, ">&", $w) or die "dup: $!" } else { open(STDERR, ">&", $w) or die "dup: $!" }
  close $w; exec { $ARGV[0] } @ARGV or die "exec: $!"'
PERL_IGNORE='my $fd = shift; $SIG{PIPE} = "IGNORE"; pipe(my $r, my $w) or die "pipe: $!"; close $r;
  if ($fd == 1) { open(STDOUT, ">&", $w) or die "dup: $!" } else { open(STDERR, ">&", $w) or die "dup: $!" }
  close $w; exec { $ARGV[0] } @ARGV or die "exec: $!"'

PASS=0
FAIL=0

assert_eq() {
    local label="$1" expected="$2" actual="$3"
    if [ "$expected" = "$actual" ]; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        echo "FAIL: $label — expected '$expected' got '$actual'" >&2
    fi
}

assert_match() {
    local label="$1" pattern="$2" actual="$3"
    if [[ "$actual" =~ $pattern ]]; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        echo "FAIL: $label — '$actual' does not match /$pattern/" >&2
    fi
}

assert_true() {
    local label="$1"; shift
    if "$@"; then PASS=$((PASS + 1)); else
        FAIL=$((FAIL + 1)); echo "FAIL: $label" >&2
    fi
}

contains() { [[ "$1" == *"$2"* ]]; }
lacks()    { [[ "$1" != *"$2"* ]]; }

# ----- sql_hex_literal ------------------------------------------------------
# The security property: whatever goes in, the SQL text it produces is drawn
# from [0-9a-f] only. No caller-supplied byte can close a string literal or add
# a dot-command argument. Needs no sqlite3, so this group always runs.
assert_eq "hex of deadbeef" "x'6465616462656566'" "$(sql_hex_literal deadbeef)"
assert_eq "hex of empty"    "x''"                 "$(sql_hex_literal "")"

HEX_ONLY="^x'[0-9a-f]*'\$"
assert_match "alphabet: sql quote payload" "$HEX_ONLY" "$(sql_hex_literal "' OR 1=1 --")"
assert_match "alphabet: drop table"        "$HEX_ONLY" "$(sql_hex_literal '"; DROP TABLE transmissions; --')"
assert_match "alphabet: backslash"         "$HEX_ONLY" "$(sql_hex_literal 'a\b')"
assert_match "alphabet: dollar and backtick" "$HEX_ONLY" "$(sql_hex_literal '$(id) `id`')"
assert_match "alphabet: embedded newline"  "$HEX_ONLY" "$(sql_hex_literal "$(printf 'a\nb')")"
assert_match "alphabet: multibyte"         "$HEX_ONLY" "$(sql_hex_literal 'héllo')"

# `od` without -v collapses runs of identical lines to '*'. A long repetitive
# value is the case that catches losing the flag.
LONG=$(printf 'x%.0s' $(seq 1 4096))
LONG_HEX=$(sql_hex_literal "$LONG")
assert_match "alphabet: 4096 repeated bytes" "$HEX_ONLY" "$LONG_HEX"
# 4096 bytes → 8192 hex digits, plus the 3 chars of x''. A collapsed run would
# be far shorter and would also fail the alphabet check on '*'.
assert_eq "no od line-collapse in 4096-byte value" "8192" "$(( ${#LONG_HEX} - 3 ))"

# ----- query text -------------------------------------------------------------
# Synthetic 32-byte pubkeys in the ingestor's form (hex.EncodeToString → lowercase).
PK_A="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
PK_B="fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
PK_ABSENT="00000000000000000000000000000000000000000000000000000000000000ff"
PK_A_UPPER=$(printf '%s' "$PK_A" | tr 'a-f' 'A-F')

COUNT_SQL=$(transmission_count_sql "$PK_A")
assert_true "query targets transmissions.from_pubkey" contains "$COUNT_SQL" "WHERE from_pubkey = lower(:pubkey)"
assert_true "query does not reference from_node"     lacks "$COUNT_SQL" "from_node"
assert_true "query text does not carry the raw pubkey" lacks "$COUNT_SQL" "$PK_A"

# ----- schema taken from the ingestor ------------------------------------------
# Pull the transmissions DDL out of cmd/ingestor/db.go instead of restating it.
transmissions_ddl() {
    awk '/CREATE TABLE IF NOT EXISTS transmissions \(/{p=1} p{print} p&&/^[[:space:]]*\);/{exit}' "$INGESTOR_DB_GO"
}
DDL=$(transmissions_ddl)
assert_match "ingestor DDL extracted" "CREATE TABLE IF NOT EXISTS transmissions \\(" "$DDL"
assert_match "ingestor DDL has from_pubkey" "from_pubkey[[:space:]]+TEXT" "$DDL"
assert_true  "ingestor DDL has no from_node" lacks "$DDL" "from_node"

# ----- against a real sqlite3 ----------------------------------------------
if ! command -v sqlite3 >/dev/null 2>&1 && [ -n "${CI:-}" ]; then
    # In CI a missing sqlite3 must not turn the binding and error-surfacing
    # groups into a silent pass.
    FAIL=$((FAIL + 1)); echo "FAIL: sqlite3 not on PATH in CI — the query group cannot run" >&2
elif ! command -v sqlite3 >/dev/null 2>&1; then
    echo "SKIP: sqlite3 not on PATH — skipping the ${#SQLITE_ARGS[@]}-flag query group" >&2
    echo "      (the alphabet, query-text and schema assertions above still ran)" >&2
else
    FIXTURE_DIR=$(mktemp -d)
    trap 'rm -rf "$FIXTURE_DIR"' EXIT
    DB="$FIXTURE_DIR/fixture.db"
    LEGACY_DB="$FIXTURE_DIR/legacy-from-node.db"
    EMPTY_DB="$FIXTURE_DIR/no-table.db"

    # Schema-realistic fixture: the ingestor's own DDL. Three ADVERTs from A, one
    # from B, and two non-ADVERT rows whose from_pubkey is NULL (the ingestor only
    # attributes ADVERTs), so a structural injection has 6 rows to leak.
    printf '%s\n' "$DDL" | sqlite3 "$DB"
    sqlite3 "$DB" <<SQL
INSERT INTO transmissions(raw_hex,hash,first_seen,payload_type,from_pubkey) VALUES
  ('00','h1','2026-01-01T00:00:00Z',4,'$PK_A'),
  ('00','h2','2026-01-01T00:00:01Z',4,'$PK_A'),
  ('00','h3','2026-01-01T00:00:02Z',4,'$PK_A'),
  ('00','h4','2026-01-01T00:00:03Z',4,'$PK_B'),
  ('00','h5','2026-01-01T00:00:04Z',5,NULL),
  ('00','h6','2026-01-01T00:00:05Z',2,NULL);
SQL
    # The fixture the previous version of this test used: an invented column.
    sqlite3 "$LEGACY_DB" "CREATE TABLE transmissions(from_node TEXT); INSERT INTO transmissions VALUES('$PK_A');"
    sqlite3 "$EMPTY_DB" "CREATE TABLE unrelated(x);"

    run_local() { sqlite3 "${SQLITE_ARGS[@]}" "$1"; }
    count() { transmission_count_sql "$1" | run_local "$2"; }

    # The capability probe must round-trip on this machine, or the assertions
    # below would be testing nothing.
    assert_eq "probe round-trips" "$SQLITE_PROBE_TOKEN" "$(sqlite_probe_sql | run_local :memory:)"
    assert_eq "fixture really holds 6 rows" "6" \
        "$(run_local "$DB" <<<'SELECT COUNT(*) FROM transmissions;')"

    # POSITIVE CONTROL: a legitimate pubkey still returns its rows. Without this,
    # a silently broken query looks like a passing security fix.
    out=$(count "$PK_A" "$DB"); rc=$?
    assert_eq "legit pubkey → its 3 rows"  "3" "$out"
    assert_eq "legit pubkey → exit 0"      "0" "$rc"
    assert_eq "other legit pubkey → 1 row" "1" "$(count "$PK_B" "$DB")"
    assert_eq "upper-case pubkey matches (blacklist is case-insensitive)" "3" "$(count "$PK_A_UPPER" "$DB")"
    assert_eq "absent pubkey → 0"          "0" "$(count "$PK_ABSENT" "$DB")"
    assert_eq "prefix of a pubkey → 0 (exact match only)" "0" "$(count "${PK_A:0:16}" "$DB")"

    # NEGATIVE: payloads bind as literals that match nothing. A structural
    # injection would return 6 (or 4, the non-NULL rows), not 0.
    for payload in "' OR 1=1 --" "') OR 1=1 --" '" OR 1=1 --' \
                   "x' OR from_pubkey IS NOT NULL --" "$PK_A' OR '1'='1" \
                   '"); .shell id; --' "$(printf "a\n.shell id\nSELECT 99;")"; do
        out=$(count "$payload" "$DB" 2>&1); rc=$?
        assert_eq "payload binds literally: $(printf %q "$payload")" "0" "$out"
        assert_eq "payload exit 0: $(printf %q "$payload")" "0" "$rc"
    done

    # Interpolating the same payload the old way returns the whole table. This is
    # the behaviour the change removes; asserting it keeps the test honest about
    # what "0" above is worth.
    legacy="SELECT COUNT(*) FROM transmissions WHERE from_pubkey = '' OR 1=1 --';"
    assert_eq "interpolated form leaks the table" "6" "$(run_local "$DB" <<<"$legacy")"

    # Multibyte, whitespace, empty and long values bind as themselves.
    LONG_PK=$(printf 'ab%.0s' $(seq 1 5000))
    sqlite3 "$DB" "INSERT INTO transmissions(raw_hex,hash,first_seen,from_pubkey) VALUES
      ('00','m1','t','héllo wörld'),('00','m2','t',''),('00','m3','t','  '),('00','m4','t','$LONG_PK');"
    assert_eq "multibyte value with a space binds" "1" "$(count 'héllo wörld' "$DB")"
    assert_eq "empty value binds as ''"            "1" "$(count '' "$DB")"
    assert_eq "whitespace value binds as itself"   "1" "$(count '  ' "$DB")"
    assert_eq "10000-byte value binds"             "1" "$(count "$LONG_PK" "$DB")"

    # REAL DATA: the committed staging-captured fixture. Its most-attributed
    # pubkey, counted by the script's query, must equal a direct count.
    if [ -f "$REAL_FIXTURE" ]; then
        cp "$REAL_FIXTURE" "$FIXTURE_DIR/real.db"
        real_pk=$(run_local "$FIXTURE_DIR/real.db" <<<"SELECT from_pubkey FROM transmissions WHERE from_pubkey IS NOT NULL GROUP BY from_pubkey ORDER BY COUNT(*) DESC, from_pubkey LIMIT 1;")
        assert_match "real fixture has an attributed pubkey" '^[0-9a-f]{64}$' "$real_pk"
        # Only interpolated into the reference count once it is known to be hex.
        [[ "$real_pk" =~ ^[0-9a-f]{64}$ ]] || real_pk=""
        real_n=$(run_local "$FIXTURE_DIR/real.db" <<<"SELECT COUNT(*) FROM transmissions WHERE from_pubkey = '$real_pk';")
        assert_match "real fixture count > 0" '^[1-9][0-9]*$' "$real_n"
        assert_eq "script query on real fixture" "$real_n" "$(count "$real_pk" "$FIXTURE_DIR/real.db")"
    else
        FAIL=$((FAIL + 1)); echo "FAIL: $REAL_FIXTURE missing" >&2
    fi

    # ERROR SURFACING: a broken query must be distinguishable from an empty
    # result — non-zero exit and something on stderr, not a silent "" or "0".
    for bad in "$LEGACY_DB:no such column: from_pubkey" "$EMPTY_DB:no such table: transmissions"; do
        bad_db=${bad%%:*}; want=${bad#*:}
        err_file="$FIXTURE_DIR/err"
        out=$(count "$PK_A" "$bad_db" 2>"$err_file"); rc=$?
        assert_true "$want → non-zero exit (got $rc)" test "$rc" -ne 0
        assert_true "$want → named on stderr" contains "$(cat "$err_file")" "$want"
        assert_eq   "$want → no count on stdout" "" "$out"
    done

    # ----- a fake target: argv, transport and the db-path contract --------------
    # The script runs unmodified against fake `ssh`, `docker` and `curl` placed
    # first on PATH. Every other command the run (or the "remote" side) executes
    # goes through a logging shim that records its argv and then execs the real
    # binary, so the argv log is what `ps` would have shown. The fake ssh runs
    # the remote command through a real `bash -c`, so the script's own quoting is
    # what gets parsed; the fake docker gives the container its own filesystem
    # root, so a container path and a host path really are different files.
    FAKE="$FIXTURE_DIR/fake"
    SHIM_DIR="$FAKE/shim"; SHIM_NOJQ_DIR="$FAKE/shim-nojq"
    FAKE_STATE="$FAKE/state"; FAKE_CROOT="$FAKE/container-root"
    HOST_DIR="$FAKE/host/var/lib/corescope"; FAKE_TMPDIR="$FAKE/tmp"
    mkdir -p "$SHIM_DIR" "$SHIM_NOJQ_DIR" "$FAKE_STATE" "$FAKE_CROOT/srv/corescope/data" \
             "$HOST_DIR" "$FAKE_TMPDIR" "$FAKE/target"
    ORIG_PATH="$PATH"

    cat >"$SHIM_DIR/logexec" <<'SHIM'
#!/bin/bash
# Record this exec's argv, then run the real command from FAKE_REAL_PATH.
name=${0##*/}
{ printf '%s' "$name"; printf ' %q' "$@"; printf '\n'; } >>"$FAKE_ARGV_LOG"
set -f; IFS=:
for d in $FAKE_REAL_PATH; do
    if [ -x "$d/$name" ] && [ ! -d "$d/$name" ]; then exec "$d/$name" "$@"; fi
done
echo "logexec: $name not found" >&2; exit 127
SHIM
    chmod +x "$SHIM_DIR/logexec"
    for c in bash sh cat grep od tr mktemp rm mv chmod chown date sleep jq python3 \
             sqlite3 head sed awk tee env ls cp; do
        ln -s logexec "$SHIM_DIR/$c"
        [ "$c" = jq ] || ln -s "$SHIM_DIR/logexec" "$SHIM_NOJQ_DIR/$c"
    done

    cat >"$SHIM_DIR/ssh" <<'FAKESSH'
#!/bin/bash
# Fake ssh: log argv, keep this call's command and stdin, run the command with
# a real `bash -c` as sshd would. Remote PATH is FAKE_REMOTE_PATH.
{ printf 'ssh'; printf ' %q' "$@"; printf '\n'; } >>"$FAKE_ARGV_LOG"
while [ $# -gt 0 ]; do
    case $1 in -i|-o|-p|-l|-F) shift 2 ;; -*) shift ;; *) break ;; esac
done
shift  # host
n=0; [ -f "$FAKE_STATE/ssh.n" ] && n=$(<"$FAKE_STATE/ssh.n"); n=$((n + 1)); echo "$n" >"$FAKE_STATE/ssh.n"
printf '%s' "$*" >"$FAKE_STATE/ssh.$n.cmd"
if [ "${FAKE_SSH_FAIL_CMD:-}" != "" ] && [[ "$*" == *"$FAKE_SSH_FAIL_CMD"* ]]; then
    fails=0; [ -f "$FAKE_STATE/ssh.failcount" ] && fails=$(<"$FAKE_STATE/ssh.failcount")
    fails=$((fails + 1)); echo "$fails" >"$FAKE_STATE/ssh.failcount"
    if [ "$fails" -ge "${FAKE_SSH_FAIL_FROM:-1}" ]; then
        echo "ssh: connect to host stub-host port 22: Connection refused" >&2; exit 255
    fi
fi
"$FAKE_REAL_TEE" "$FAKE_STATE/ssh.$n.stdin" | PATH="$FAKE_REMOTE_PATH" bash -c "$*"
exit "${PIPESTATUS[1]}"
FAKESSH

    cat >"$SHIM_DIR/docker" <<'FAKEDOCKER'
#!/bin/bash
# Fake docker: `restart` snapshots the config's blacklist as the app's live
# state; `exec` runs the command with absolute paths mapped into the container
# root. Without FAKE_CONTAINER_SQLITE=1 the container has no sqlite3, like the
# production image.
{ printf 'docker'; printf ' %q' "$@"; printf '\n'; } >>"$FAKE_ARGV_LOG"
case $1 in
    restart)
        [ "$2" = "$FAKE_CONTAINER" ] || { echo "Error: No such container: $2" >&2; exit 1; }
        "$FAKE_REAL_JQ" -r '.nodeBlacklist // [] | .[] | ascii_downcase' "$FAKE_CONFIG" \
            >"$FAKE_STATE/live-blacklist" || exit 1
        { echo "restart"; cat "$FAKE_STATE/live-blacklist"; } >>"$FAKE_STATE/restarts" 2>/dev/null
        echo "$2" ;;
    exec)
        shift; [ "$1" = -i ] && shift
        [ "$1" = "$FAKE_CONTAINER" ] || { echo "Error: No such container: $1" >&2; exit 1; }
        shift
        if [ "$1" = sqlite3 ] && [ "${FAKE_CONTAINER_SQLITE:-0}" != 1 ]; then
            echo 'OCI runtime exec failed: exec: "sqlite3": executable file not found in $PATH' >&2
            exit 126
        fi
        args=()
        for a in "$@"; do
            case $a in /dev/*) args+=("$a") ;; /*) args+=("$FAKE_CROOT$a") ;; *) args+=("$a") ;; esac
        done
        exec "${args[@]}" ;;
    *) echo "fake docker: unsupported: $1" >&2; exit 1 ;;
esac
FAKEDOCKER

    cat >"$SHIM_DIR/curl" <<'FAKECURL'
#!/bin/bash
# Fake curl + CoreScope API, in pure bash so the fake's own work adds nothing to
# the argv log. Takes the URL from argv or from a -K - config on stdin. Can
# send a signal to the script when a given path is hit for the Nth time.
{ printf 'curl'; printf ' %q' "$@"; printf '\n'; } >>"$FAKE_ARGV_LOG"
out=/dev/stdout; fmt=""; url=""; cfg=""
while [ $# -gt 0 ]; do
    case $1 in
        -s) shift ;;
        -m|-H) shift 2 ;;
        -o) out=$2; shift 2 ;;
        -w) fmt=$2; shift 2 ;;
        -K) cfg=$2; shift 2 ;;
        -*) shift ;;
        *) url=$1; shift ;;
    esac
done
if [ "$cfg" = - ]; then
    while IFS= read -r line; do
        case $line in 'url = "'*'"') url=${line#'url = "'}; url=${url%'"'} ;; esac
    done
fi
path=${url#"$FAKE_URL"}
if [ "$path" = "$url" ]; then printf '000'; exit 7; fi

hits_file="$FAKE_STATE/hits.${path//[^a-z]/_}"
hits=0; [ -f "$hits_file" ] && hits=$(<"$hits_file"); hits=$((hits + 1)); echo "$hits" >"$hits_file"
if [ -n "${FAKE_SIGNAL_ON:-}" ] && [ "$path" = "$FAKE_SIGNAL_ON" ] && [ "$hits" = "${FAKE_SIGNAL_NTH:-1}" ]; then
    kill -s "$FAKE_SIGNAL" "$(<"$FAKE_STATE/script.pid")"
fi

BL=(); while IFS= read -r b; do BL+=("$b"); done <"$FAKE_STATE/live-blacklist"
hidden() {
    [ "${FAKE_LEAK:-0}" = 1 ] && return 1
    [ "${FAKE_DETAIL_LEAK:-0}" = 1 ] && [ "${path#/api/nodes/}" != "$path" ] && return 1
    local b; for b in ${BL[@]+"${BL[@]}"}; do [ "$b" = "$1" ] && return 0; done; return 1
}
code=404; body='{"error":"not found"}'
case $path in
    /api/stats) code=200; body='{"ok":true}' ;;
    /api/nodes/*)
        want=${path#/api/nodes/}
        for n in $FAKE_NODES; do
            if [ "$n" = "$want" ] && ! hidden "$n"; then code=200; body="{\"public_key\":\"$n\"}"; fi
        done ;;
    '/api/nodes?limit=10000')
        code=${FAKE_LIST_CODE:-200}; body='{"nodes":['; sep=''
        for n in $FAKE_NODES; do hidden "$n" && continue; body+="$sep{\"public_key\":\"$n\"}"; sep=','; done
        body+=']}' ;;
    /api/topology)
        code=200; body='{"nodes":['; sep=''
        for n in $FAKE_NODES; do hidden "$n" && continue; body+="$sep\"$n\""; sep=','; done
        body+=']}' ;;
esac
printf '%s' "$body" >"$out"
[ "$fmt" = '%{http_code}' ] && printf '%s' "$code"
exit 0
FAKECURL
    chmod +x "$SHIM_DIR/ssh" "$SHIM_DIR/docker" "$SHIM_DIR/curl"
    for c in ssh docker curl; do ln -s "$SHIM_DIR/$c" "$SHIM_NOJQ_DIR/$c"; done

    # Two databases with DIFFERENT counts for PK_A, so the count names the file
    # that was read: the container's holds 2, the host's holds 3.
    CONTAINER_DB_PATH="/srv/corescope/data/meshcore.db"   # a path inside the container
    HOST_DB_PATH="$HOST_DIR/meshcore.db"                  # a path on the host
    printf '%s\n' "$DDL" | sqlite3 "$FAKE_CROOT$CONTAINER_DB_PATH"
    sqlite3 "$FAKE_CROOT$CONTAINER_DB_PATH" "INSERT INTO transmissions(raw_hex,hash,first_seen,payload_type,from_pubkey) VALUES
      ('00','c1','t',4,'$PK_A'),('00','c2','t',4,'$PK_A'),('00','c3','t',4,'$PK_B');"
    cp "$DB" "$HOST_DB_PATH"
    : >"$FAKE_CROOT/srv/corescope/data/empty.db"
    cp "$LEGACY_DB" "$HOST_DIR/legacy.db"
    UNBINDABLE_BIN="$FIXTURE_DIR/unbindable-bin"; mkdir -p "$UNBINDABLE_BIN"
    cat >"$UNBINDABLE_BIN/sqlite3" <<'FAKE'
#!/usr/bin/env bash
# A sqlite3 whose .parameter cannot bind: help text on stdout, exit 0.
cat >/dev/null
echo ".parameter CMD ...       Manage SQL parameter bindings"
echo "0"
FAKE
    chmod +x "$UNBINDABLE_BIN/sqlite3"
    NOISY_BIN="$FIXTURE_DIR/noisy-bin"; mkdir -p "$NOISY_BIN"
    cat >"$NOISY_BIN/sqlite3" <<'FAKE'
#!/usr/bin/env bash
# Echoes the probe token among other output without having bound anything.
cat >/dev/null
echo ".parameter CMD ...       Manage SQL parameter bindings"
echo "corescope-probe-ok"
FAKE
    chmod +x "$NOISY_BIN/sqlite3"

    FAKE_CONFIG="$FAKE/target/config.json"
    ORIG_CONFIG='{"port":3000,"nodeBlacklist":["aa00aa00"],"mqtt":{"sources":[]}}'
    SYNTH_TOKEN="synthetic-admin-token-not-a-secret-83"
    export FAKE_ARGV_LOG="$FAKE/argv.log" FAKE_STATE FAKE_CROOT FAKE_CONFIG
    export FAKE_REAL_TEE; FAKE_REAL_TEE=$(command -v tee)
    export FAKE_REAL_JQ; FAKE_REAL_JQ=$(command -v jq)
    export FAKE_CONTAINER="corescope-stub" FAKE_URL="http://target.invalid"
    export FAKE_NODES="$PK_A $PK_B"
    ALL_ARGV="$FAKE/all-argv.log"; : >"$ALL_ARGV"
    RUN_N=0

    snapshot_files() { (cd "$FAKE" && find host container-root tmp target -print | LC_ALL=C sort); }
    db_sums() { cksum "$FAKE_CROOT$CONTAINER_DB_PATH" "$HOST_DB_PATH" | awk '{print $1, $2}'; }
    DB_SUMS=$(db_sums)

    # Run the whole script against the fake target. Settings come as NAME=VALUE
    # arguments; `--` then an optional wrapper command (e.g. a stream breaker).
    # Sets RUN_RC, RUN_OUT, RUN_ERR.
    run_full() {
        local kv wrap=()
        rm -rf "$FAKE_STATE"; mkdir -p "$FAKE_STATE"; : >"$FAKE_STATE/live-blacklist"
        printf '%s\n' "$ORIG_CONFIG" >"$FAKE_CONFIG"
        : >"$FAKE_ARGV_LOG"
        FILES_BEFORE=$(snapshot_files)
        RUN_N=$((RUN_N + 1))
        (
            unset TARGET_DB_PATH ADMIN_API_TOKEN TARGET_CONTAINER_DB_PATH TARGET_HOST_DB_PATH \
                  FAKE_CONTAINER_SQLITE FAKE_SIGNAL_ON FAKE_SIGNAL FAKE_SIGNAL_NTH FAKE_LEAK \
                  FAKE_DETAIL_LEAK FAKE_LIST_CODE \
                  FAKE_SSH_FAIL_CMD FAKE_SSH_FAIL_FROM
            export TEST_NODE_PUBKEY="$PK_A" TARGET_SSH_HOST="stub-host" TARGET_SSH_KEY="/nonexistent/key" \
                   TARGET_CONFIG_PATH="$FAKE_CONFIG" TARGET_CONTAINER="$FAKE_CONTAINER" \
                   CURL_TIMEOUT=2 RESTART_WAIT_S=4 TMPDIR="$FAKE_TMPDIR" \
                   FAKE_REAL_PATH="$ORIG_PATH" FAKE_REMOTE_PATH="$SHIM_DIR"
            while [ $# -gt 0 ] && [ "$1" != -- ]; do kv=$1; export "${kv?}"; shift; done
            [ "${1:-}" = -- ] && shift
            wrap=("$@")
            export PATH="$SHIM_DIR:$ORIG_PATH"
            # Optional kernel-level evidence (Linux): strace every execve of the
            # run, including anything the PATH shims cannot see.
            local tracer=()
            if [ -n "${BLACKLIST_TEST_STRACE_DIR:-}" ]; then
                tracer=(strace -f -qq -e trace=execve -s 65535 -o "$BLACKLIST_TEST_STRACE_DIR/run.$RUN_N")
            fi
            # Foreground, so SIGINT is not ignored the way it is for a background
            # job; the pid is recorded for the fake curl to signal.
            ${tracer[@]+"${tracer[@]}"} ${wrap[@]+"${wrap[@]}"} bash -c 'echo $$ >"$FAKE_STATE/script.pid"; exec bash "$@"' _ \
                "$BLACKLIST_SH" "http://baseline.invalid" "$FAKE_URL" \
                >"$FAKE/out" 2>"$FAKE/err"
        )
        RUN_RC=$?
        RUN_OUT=$(cat "$FAKE/out"); RUN_ERR=$(cat "$FAKE/err")
        cat "$FAKE_ARGV_LOG" >>"$ALL_ARGV"
    }
    restarts() {
        if [ -f "$FAKE_STATE/restarts" ]; then awk '$0 == "restart" { n++ } END { print n + 0 }' "$FAKE_STATE/restarts"
        else echo 0; fi
    }
    config_restored() { [ "$(jq -S . "$FAKE_CONFIG")" = "$(printf '%s' "$ORIG_CONFIG" | jq -S .)" ]; }
    files_unchanged() { [ "$(snapshot_files)" = "$FILES_BEFORE" ]; }
    argv_has()        { grep -qF -- "$1" "$FAKE_ARGV_LOG"; }
    # The ssh call whose command contains $1: prints its number.
    ssh_call() {
        local n=1
        while [ -f "$FAKE_STATE/ssh.$n.cmd" ]; do
            if [[ "$(cat "$FAKE_STATE/ssh.$n.cmd")" == *"$1"* ]]; then echo "$n"; return 0; fi
            n=$((n + 1))
        done
        return 1
    }
    # Assertions every run must meet: target restored exactly, nothing left
    # behind, and nothing sensitive in any process's argv.
    common_after() {
        local label="$1"
        assert_true "$label: config restored"             config_restored
        assert_eq   "$label: databases unchanged"         "$DB_SUMS" "$(db_sums)"
        assert_true "$label: no new or leftover files"    files_unchanged
        assert_true "$label: argv log recorded the run"   test -s "$FAKE_ARGV_LOG"
        assert_true "$label: pubkey not in any argv"      lacks "$(cat "$FAKE_ARGV_LOG")" "$PK_A"
        assert_true "$label: SQL not in any argv"         lacks "$(cat "$FAKE_ARGV_LOG")" "SELECT"
        assert_true "$label: token not in any argv"       lacks "$(cat "$FAKE_ARGV_LOG")" "$SYNTH_TOKEN"
        assert_true "$label: remote input exactly as built" remote_input_clean
    }
    # Every ssh call carries only what the script built: config edits have the
    # fixed command line and start their stdin with the read line; sqlite3
    # calls start their stdin with .parameter init. Stray output (bash 3.2's
    # leftover stdout buffer) shows up here as extra leading lines.
    remote_input_clean() {
        local n=1 cmd first
        while [ -f "$FAKE_STATE/ssh.$n.cmd" ]; do
            cmd=$(cat "$FAKE_STATE/ssh.$n.cmd"); first=$(head -n 1 "$FAKE_STATE/ssh.$n.stdin")
            case $cmd in
                *"bash -s")
                    [[ "$cmd" =~ ^CFG=[^[:space:]]+\ MODE=(add|remove)\ bash\ -s$ ]] || { echo "  unexpected edit cmd: $cmd" >&2; return 1; }
                    [ "$first" = "IFS= read -r PK || exit 3" ] || { echo "  unexpected edit stdin: $first" >&2; return 1; } ;;
                *sqlite3*)
                    [ "$first" = ".parameter init" ] || { echo "  unexpected sqlite stdin: $first" >&2; return 1; } ;;
                *)
                    [ ! -s "$FAKE_STATE/ssh.$n.stdin" ] || { echo "  unexpected stdin for: $cmd" >&2; return 1; } ;;
            esac
            n=$((n + 1))
        done
    }

    # 1. Container has no sqlite3 → host fallback with the HOST path (count 3).
    #    Container and host paths differ. Also the stdin-transport evidence.
    run_full TARGET_CONTAINER_DB_PATH="$CONTAINER_DB_PATH" TARGET_HOST_DB_PATH="$HOST_DB_PATH"
    assert_eq   "host fallback: exit 0" "0" "$RUN_RC"
    assert_true "host fallback: runner named" contains "$RUN_OUT" "sqlite3 runner: host (TARGET_HOST_DB_PATH)"
    assert_true "host fallback: host db count" contains "$RUN_OUT" "DB retains 3 packets"
    assert_true "host fallback: hidden"        contains "$RUN_OUT" "hide ok: detail=404 in_list=0"
    assert_true "host fallback: topology"      contains "$RUN_OUT" "topology clean"
    assert_true "host fallback: teardown ok"   contains "$RUN_OUT" "teardown ok"
    assert_eq   "host fallback: two restarts"  "2" "$(restarts)"
    # The pubkey reached the app through the stdin edit: the first restart saw
    # it in the blacklist, the second (teardown) did not.
    assert_eq   "stdin edit: added, then removed" "restart $PK_A aa00aa00 restart aa00aa00" \
                "$(tr '\n' ' ' <"$FAKE_STATE/restarts" | sed 's/ $//')"
    common_after "host fallback"
    assert_true "host fallback: container path never used" lacks "$(cat "$FAKE_ARGV_LOG")" "$CONTAINER_DB_PATH"
    assert_true "host fallback: host path used"      argv_has "$HOST_DB_PATH"
    assert_true "host fallback: container tried first" contains "$(cat "$FAKE_STATE/ssh.$(ssh_call ':memory:').cmd")" "docker exec -i corescope-stub sqlite3"
    # Coverage of the argv log itself: every kind of call is in it.
    for c in 'ssh ' 'curl ' 'grep ' 'jq ' 'sqlite3 ' 'docker ' 'bash -c'; do
        assert_true "argv log covers: $c" grep -q "^$c" "$FAKE_ARGV_LOG"
    done
    assert_true "grep reads its pattern from a file" grep -q '^grep -qF -f .*pubkey' "$FAKE_ARGV_LOG"
    assert_true "curl reads its URL from stdin"       grep -q '^curl .*-K -' "$FAKE_ARGV_LOG"
    assert_true "no URL in curl argv"                 lacks "$(grep '^curl ' "$FAKE_ARGV_LOG")" "$FAKE_URL"
    edit=$(ssh_call "bash -s")
    assert_eq   "config-edit command carries no value" \
                "CFG=$FAKE_CONFIG MODE=add bash -s" "$(cat "$FAKE_STATE/ssh.$edit.cmd")"
    assert_eq   "config-edit stdin: pubkey is line 2" "$PK_A" "$(sed -n 2p "$FAKE_STATE/ssh.$edit.stdin")"
    query=$(ssh_call "-readonly $HOST_DB_PATH")
    qin=$(cat "$FAKE_STATE/ssh.$query.stdin")
    assert_true "sql stdin: the count query"    contains "$qin" "SELECT COUNT(*) FROM transmissions WHERE from_pubkey = lower(:pubkey);"
    assert_true "sql stdin: pubkey hex-encoded" contains "$qin" "$(sql_hex_literal "$PK_A")"
    assert_true "sql stdin: raw pubkey absent"  lacks "$qin" "$PK_A"
    assert_true "path check before query" test "$(ssh_call "test -f $HOST_DB_PATH")" -lt "$query"

    # 2. Container has sqlite3 → container runner with the CONTAINER path
    #    (count 2). A valid host db is configured too and must not be read.
    run_full TARGET_CONTAINER_DB_PATH="$CONTAINER_DB_PATH" TARGET_HOST_DB_PATH="$HOST_DB_PATH" FAKE_CONTAINER_SQLITE=1
    assert_eq   "container runner: exit 0" "0" "$RUN_RC"
    assert_true "container runner: named" contains "$RUN_OUT" "sqlite3 runner: container (TARGET_CONTAINER_DB_PATH)"
    assert_true "container runner: container db count" contains "$RUN_OUT" "DB retains 2 packets"
    assert_true "container runner: host path never used" lacks "$(cat "$FAKE_ARGV_LOG")" "$HOST_DB_PATH"
    assert_true "container runner: path checked in the container" \
                argv_has "docker exec corescope-stub sh -c"
    common_after "container runner"

    # 3. Only a container path, container has no sqlite3 → fail; the host is
    #    never tried with the container's path.
    run_full TARGET_CONTAINER_DB_PATH="$CONTAINER_DB_PATH"
    assert_eq   "container path only: exit 1" "1" "$RUN_RC"
    assert_true "container path only: classified" contains "$RUN_OUT" "retain-failed: no sqlite3 able to bind"
    assert_true "container path only: probe error shown" contains "$RUN_ERR" '"sqlite3": executable file not found'
    assert_true "container path only: host not probed" lacks "$(grep '^bash -c sqlite3' "$FAKE_ARGV_LOG")" "sqlite3"
    common_after "container path only"

    # 4. Host path that does not exist → named as such before any query, and
    #    no empty database appears.
    run_full TARGET_HOST_DB_PATH="$HOST_DIR/missing.db"
    assert_eq   "wrong host path: exit 1" "1" "$RUN_RC"
    assert_true "wrong host path: classified" contains "$RUN_OUT" "TARGET_HOST_DB_PATH=$HOST_DIR/missing.db is not a non-empty file in the host"
    assert_true "wrong host path: no file created" test ! -e "$HOST_DIR/missing.db"
    assert_true "wrong host path: sqlite3 never opened it" lacks "$(grep '^sqlite3 ' "$FAKE_ARGV_LOG")" "missing.db"
    common_after "wrong host path"

    # 5. Wrong container path with a sqlite3-capable container → no fallback to
    #    the host, no file created in the container.
    run_full TARGET_CONTAINER_DB_PATH="/srv/corescope/data/missing.db" TARGET_HOST_DB_PATH="$HOST_DB_PATH" FAKE_CONTAINER_SQLITE=1
    assert_eq   "wrong container path: exit 1" "1" "$RUN_RC"
    assert_true "wrong container path: classified" contains "$RUN_OUT" "TARGET_CONTAINER_DB_PATH=/srv/corescope/data/missing.db is not a non-empty file in the container"
    assert_true "wrong container path: no file created" test ! -e "$FAKE_CROOT/srv/corescope/data/missing.db"
    assert_true "wrong container path: host db not read instead" lacks "$(cat "$FAKE_ARGV_LOG")" "$HOST_DB_PATH"
    common_after "wrong container path"

    # 6. No path at all → classified, no sqlite3 anywhere.
    run_full
    assert_eq   "no db path: exit 1" "1" "$RUN_RC"
    assert_true "no db path: classified" contains "$RUN_OUT" "retain-failed: no db path"
    assert_true "no db path: sqlite3 never run" lacks "$(cat "$FAKE_ARGV_LOG")" "sqlite3"
    common_after "no db path"

    # 7. Query error (a db without from_pubkey) → failure with no count.
    run_full TARGET_HOST_DB_PATH="$HOST_DIR/legacy.db"
    assert_eq   "query error: exit 1" "1" "$RUN_RC"
    assert_true "query error: classified" contains "$RUN_OUT" "retain-failed: sqlite3 query failed via host"
    assert_true "query error: sqlite error kept" contains "$RUN_ERR" "no such column: from_pubkey"
    assert_true "query error: no count claimed" lacks "$RUN_OUT" "DB retains"
    common_after "query error"

    # 8. A sqlite3 that cannot bind → rejected by the probe, not trusted.
    run_full TARGET_HOST_DB_PATH="$HOST_DB_PATH" FAKE_REAL_PATH="$UNBINDABLE_BIN:$ORIG_PATH"
    assert_eq   "unbindable sqlite3: exit 1" "1" "$RUN_RC"
    assert_true "unbindable sqlite3: classified" contains "$RUN_OUT" "retain-failed: no sqlite3 able to bind"
    common_after "unbindable sqlite3"

    # 9. No jq on the target → python3 edit path, pubkey via environment.
    run_full TARGET_HOST_DB_PATH="$HOST_DB_PATH" FAKE_REMOTE_PATH="$SHIM_NOJQ_DIR"
    assert_eq   "python3 edit: exit 0" "0" "$RUN_RC"
    assert_true "python3 edit: python3 did the edit" grep -q '^python3 - ' "$FAKE_ARGV_LOG"
    assert_true "python3 edit: jq not used remotely" lacks "$(cat "$FAKE_ARGV_LOG")" "jq "
    common_after "python3 edit"

    # 10. Removed variables refuse to start: nothing runs against the target.
    for removed in "TARGET_DB_PATH=$HOST_DB_PATH" "ADMIN_API_TOKEN=$SYNTH_TOKEN"; do
        name=${removed%%=*}
        run_full TARGET_HOST_DB_PATH="$HOST_DB_PATH" "$removed"
        assert_eq   "$name: exit 2" "2" "$RUN_RC"
        assert_true "$name: named as removed" contains "$RUN_ERR" "$name is no longer read"
        assert_true "$name: no ssh" lacks "$(cat "$FAKE_ARGV_LOG")" "ssh "
        assert_true "$name: no curl" lacks "$(cat "$FAKE_ARGV_LOG")" "curl "
        assert_eq   "$name: no restart" "0" "$(restarts)"
        assert_true "$name: value never printed" lacks "$RUN_OUT$RUN_ERR" "${removed#*=}"
        common_after "$name"
    done
    # Only the refusal message may still name the endpoint; nothing calls it.
    assert_true "admin API path gone: no Authorization header" lacks "$(cat "$BLACKLIST_SH")" "Authorization"
    assert_true "admin API path gone: no admin query"          lacks "$(cat "$BLACKLIST_SH")" "api/admin/transmissions?"
    assert_true "admin API path gone: token never read"        lacks "$(grep -v 'ADMIN_API_TOKEN:-}" ]]; then' "$BLACKLIST_SH" | grep -v '^ *#' | grep -v 'warn "')" 'ADMIN_API_TOKEN'


    # 11. Hide failure on the server side is reported and still torn down.
    run_full TARGET_HOST_DB_PATH="$HOST_DB_PATH" FAKE_LEAK=1
    assert_eq   "server leaks: exit 2" "2" "$RUN_RC"
    assert_true "server leaks: hide-failed" contains "$RUN_OUT" "hide-failed: detail=200 in_list=1"
    assert_true "server leaks: topology hide-failed" contains "$RUN_OUT" "/api/topology references blacklisted pubkey"
    common_after "server leaks"

    # 11b. grep itself failing is a failure, never "pubkey absent" (which, for
    #      a hide check, is a pass).
    GREP_ERR_BIN="$FIXTURE_DIR/grep-err-bin"; mkdir -p "$GREP_ERR_BIN"
    printf '#!/bin/sh\necho "grep: simulated I/O error" >&2\nexit 2\n' >"$GREP_ERR_BIN/grep"
    chmod +x "$GREP_ERR_BIN/grep"
    run_full TARGET_HOST_DB_PATH="$HOST_DB_PATH" FAKE_REAL_PATH="$GREP_ERR_BIN:$ORIG_PATH"
    assert_eq   "grep error: both searches fail" "2" "$RUN_RC"
    assert_true "grep error: list classified" contains "$RUN_OUT" "could not search /api/nodes response"
    assert_true "grep error: topology classified" contains "$RUN_OUT" "could not search /api/topology response"
    assert_true "grep error: no hide ok claimed" lacks "$RUN_OUT" "hide ok"
    common_after "grep error"

    # 11c. Hidden means detail 404 AND absent from the listing; a listing that
    #      could not be fetched proves nothing.
    run_full TARGET_HOST_DB_PATH="$HOST_DB_PATH" FAKE_LIST_CODE=500
    assert_eq   "listing HTTP 500: fails" "1" "$RUN_RC"
    assert_true "listing HTTP 500: classified" contains "$RUN_OUT" "hide-failed: /api/nodes HTTP 500"
    common_after "listing HTTP 500"
    run_full TARGET_HOST_DB_PATH="$HOST_DB_PATH" FAKE_DETAIL_LEAK=1
    assert_eq   "detail leaks, listing hides: fails" "1" "$RUN_RC"
    assert_true "detail leaks, listing hides: classified" contains "$RUN_OUT" "hide-failed: detail=200 in_list=0"
    common_after "detail leaks, listing hides"

    # 11d. Empty file at the container path, sqlite3-capable container →
    #      refused before any query; the file stays empty.
    run_full TARGET_CONTAINER_DB_PATH="/srv/corescope/data/empty.db" FAKE_CONTAINER_SQLITE=1
    assert_eq   "empty container db: exit 1" "1" "$RUN_RC"
    assert_true "empty container db: classified" contains "$RUN_OUT" "TARGET_CONTAINER_DB_PATH=/srv/corescope/data/empty.db is not a non-empty file in the container"
    assert_true "empty container db: still empty" test ! -s "$FAKE_CROOT/srv/corescope/data/empty.db"
    assert_true "empty container db: sqlite3 never opened it" lacks "$(grep '^sqlite3 ' "$FAKE_ARGV_LOG")" "empty.db"
    common_after "empty container db"

    # 11e. The probe wants the token back and nothing else.
    run_full TARGET_HOST_DB_PATH="$HOST_DB_PATH" FAKE_REAL_PATH="$NOISY_BIN:$ORIG_PATH"
    assert_eq   "noisy sqlite3: rejected" "1" "$RUN_RC"
    assert_true "noisy sqlite3: classified" contains "$RUN_OUT" "retain-failed: no sqlite3 able to bind"
    common_after "noisy sqlite3"

    # 12. Signals mid-run: classified exit, full teardown.
    for sig in TERM:143 INT:130 HUP:129; do
        run_full TARGET_HOST_DB_PATH="$HOST_DB_PATH" FAKE_SIGNAL_ON=/api/topology FAKE_SIGNAL="${sig%%:*}"
        assert_eq   "SIG${sig%%:*} mid-run: exit ${sig#*:}" "${sig#*:}" "$RUN_RC"
        assert_true "SIG${sig%%:*} mid-run: teardown ok" contains "$RUN_OUT" "teardown ok"
        assert_eq   "SIG${sig%%:*} mid-run: two restarts" "2" "$(restarts)"
        common_after "SIG${sig%%:*} mid-run"
    done

    # 13. SIGTERM mid-run and a failing teardown restart → 143 + 1.
    run_full TARGET_HOST_DB_PATH="$HOST_DB_PATH" FAKE_SIGNAL_ON=/api/topology FAKE_SIGNAL=TERM \
             FAKE_SSH_FAIL_CMD="docker restart" FAKE_SSH_FAIL_FROM=2
    assert_eq   "SIGTERM + failed teardown: exit 144" "144" "$RUN_RC"
    assert_true "SIGTERM + failed teardown: classified" contains "$RUN_OUT" "teardown-failed"

    # 14. A second signal during teardown's stats wait: the restore finishes
    #     and the run's status is kept.
    run_full TARGET_HOST_DB_PATH="$HOST_DB_PATH" FAKE_SIGNAL_ON=/api/stats FAKE_SIGNAL_NTH=2 FAKE_SIGNAL=INT
    assert_eq   "INT in teardown: status kept" "0" "$RUN_RC"
    assert_true "INT in teardown: noted" contains "$RUN_ERR" "teardown continues"
    assert_true "INT in teardown: teardown ok" contains "$RUN_OUT" "teardown ok"
    common_after "INT in teardown"

    # 15. Broken output streams (see the stream-breaker below).
    if command -v perl >/dev/null 2>&1; then
        # Only stderr broken, failing run: diagnostics are lost, the status and
        # the restore are not.
        run_full TARGET_HOST_DB_PATH="$HOST_DIR/legacy.db" -- perl -e "$PERL_BREAK" 2
        assert_eq   "stderr broken, query error: exit 1" "1" "$RUN_RC"
        assert_true "stderr broken, query error: teardown ok" contains "$RUN_OUT" "teardown ok"
        common_after "stderr broken, query error"
        # Only stderr broken and a signal in teardown: the notice goes to the
        # broken stderr. Before #83 that SIGPIPE killed the shell mid-restore.
        run_full TARGET_HOST_DB_PATH="$HOST_DB_PATH" FAKE_SIGNAL_ON=/api/stats FAKE_SIGNAL_NTH=2 FAKE_SIGNAL=INT \
                 -- perl -e "$PERL_BREAK" 2
        assert_eq   "stderr broken, INT in teardown: status kept" "0" "$RUN_RC"
        assert_true "stderr broken, INT in teardown: teardown ok" contains "$RUN_OUT" "teardown ok"
        common_after "stderr broken, INT in teardown"
        # stdout broken from the start: the first echo raises SIGPIPE → 141,
        # after a teardown that still restores (one restart: teardown's).
        run_full TARGET_HOST_DB_PATH="$HOST_DB_PATH" -- perl -e "$PERL_BREAK" 1
        assert_eq   "stdout broken: exit 141" "141" "$RUN_RC"
        assert_eq   "stdout broken: teardown restarted" "1" "$(restarts)"
        common_after "stdout broken"
        # stdout broken AND SIGPIPE ignored from the start (not trappable): the
        # run cannot stop, so every message fails. On bash 3.2 the unwritten
        # text used to be replayed into the next subshell — the config-edit
        # group piped to ssh, whose remote bash then ran it. The run must keep
        # its real result and send nothing but what it built.
        run_full TARGET_HOST_DB_PATH="$HOST_DB_PATH" -- perl -e "$PERL_IGNORE" 1
        assert_eq   "stdout dead, SIGPIPE ignored: real result" "0" "$RUN_RC"
        assert_eq   "stdout dead, SIGPIPE ignored: two restarts" "2" "$(restarts)"
        common_after "stdout dead, SIGPIPE ignored"
        run_full TARGET_HOST_DB_PATH="$HOST_DIR/legacy.db" -- perl -e "$PERL_IGNORE" 2
        assert_eq   "stderr dead, SIGPIPE ignored: real result" "1" "$RUN_RC"
        assert_true "stderr dead, SIGPIPE ignored: stdout intact" contains "$RUN_OUT" "teardown ok"
        common_after "stderr dead, SIGPIPE ignored"
    elif [ -n "${CI:-}" ]; then
        FAIL=$((FAIL + 1)); echo "FAIL: perl not on PATH in CI — the broken-stream group cannot run" >&2
    else
        echo "SKIP: perl not on PATH — skipping the full-run broken-stream group" >&2
    fi

    # Across every run above.
    assert_true "all runs: pubkey never in argv" lacks "$(cat "$ALL_ARGV")" "$PK_A"
    assert_true "all runs: token never in argv"  lacks "$(cat "$ALL_ARGV")" "$SYNTH_TOKEN"
    assert_true "all runs: SQL never in argv"    lacks "$(cat "$ALL_ARGV")" "from_pubkey"
    if [ -n "${BLACKLIST_TEST_STRACE_DIR:-}" ]; then
        traces=$(cat "$BLACKLIST_TEST_STRACE_DIR"/run.* 2>/dev/null)
        assert_eq   "strace: one trace per run" "$RUN_N" "$(find "$BLACKLIST_TEST_STRACE_DIR" -name 'run.*' | wc -l | tr -d ' ')"
        assert_true "strace: traced ssh/curl/sqlite3/jq execs" contains "$traces" 'execve("'
        for c in /ssh /curl /sqlite3 /jq /grep; do assert_true "strace: saw $c" contains "$traces" "$c\""; done
        assert_true "strace: pubkey in no execve" lacks "$traces" "$PK_A"
        assert_true "strace: SQL in no execve"    lacks "$traces" "from_pubkey"
        assert_true "strace: token in no execve"  lacks "$traces" "$SYNTH_TOKEN"
    fi

    # ----- the two empty-file guards, each on its own ---------------------------
    # db_path_ok and -readonly are independent: each alone must stop sqlite3
    # from creating a file at a wrong path.
    with_fake() {
        ( export PATH="$SHIM_DIR:$ORIG_PATH" FAKE_REAL_PATH="$ORIG_PATH" FAKE_REMOTE_PATH="$SHIM_DIR"
          rm -rf "$FAKE_STATE"; mkdir -p "$FAKE_STATE"; "$@" )
    }
    TMP="$FIXTURE_DIR"; TARGET_CONTAINER="$FAKE_CONTAINER"; TARGET_SSH_HOST="stub-host"
    TEST_PUBKEY="$PK_A"; SSH_OPTS=(-o BatchMode=yes); TARGET_CONTAINER_DB_PATH=""
    SQLITE_RUNNER=host; TARGET_HOST_DB_PATH="$HOST_DIR/direct.db"
    with_fake run_sqlite <<<"SELECT 1;" >/dev/null 2>&1; rc=$?
    assert_true "-readonly: query on a missing path fails" test "$rc" -ne 0
    assert_true "-readonly: no file created" test ! -e "$HOST_DIR/direct.db"
    assert_true "path check: missing path rejected" test "$(with_fake db_path_ok; echo $?)" -ne 0
    : >"$HOST_DIR/empty.db"; TARGET_HOST_DB_PATH="$HOST_DIR/empty.db"
    assert_true "path check: empty file rejected" test "$(with_fake db_path_ok; echo $?)" -ne 0
    rm -f "$HOST_DIR/empty.db"
    TARGET_HOST_DB_PATH="$HOST_DB_PATH"
    assert_eq   "path check: real db accepted" "0" "$(with_fake db_path_ok; echo $?)"

    # read_retain_count's own contract: a failed query leaves no count and
    # returns 1 — never a count of 0 that a caller could read as a result.
    rrc() { read_retain_count >/dev/null 2>&1; echo "$?:$RETAIN_COUNT"; }
    TARGET_HOST_DB_PATH="$HOST_DIR/legacy.db"
    assert_eq "read_retain_count: query error → 1, no count" "1:" "$(with_fake rrc)"
    TARGET_HOST_DB_PATH="$HOST_DB_PATH"
    assert_eq "read_retain_count: host db → 0, count 3" "0:3" "$(with_fake rrc)"
    TARGET_HOST_DB_PATH=""
    assert_eq "read_retain_count: no path → 1, no count" "1:" "$(with_fake rrc)"
    TARGET_HOST_DB_PATH="$HOST_DB_PATH"

    # The remote edit re-checks the pubkey it reads from stdin: a non-hex value
    # (main() never lets one through) is refused and the config left alone.
    printf '%s\n' "$ORIG_CONFIG" >"$FAKE_CONFIG"; before_cfg=$(cat "$FAKE_CONFIG")
    TARGET_CONFIG_PATH="$FAKE_CONFIG"; TEST_PUBKEY='zz"; touch /tmp/pr83-never'
    with_fake set_blacklist_state add >/dev/null 2>&1; rc=$?
    assert_eq   "remote hex check: refused" "1" "$rc"
    assert_eq   "remote hex check: config untouched" "$before_cfg" "$(cat "$FAKE_CONFIG")"
    TEST_PUBKEY="$PK_A"

    # run_sqlite with no resolved runner must refuse rather than guess.
    SQLITE_RUNNER=""
    if run_sqlite </dev/null >/dev/null 2>&1; then
        FAIL=$((FAIL + 1)); echo "FAIL: run_sqlite with no runner — expected non-zero exit" >&2
    else
        PASS=$((PASS + 1))
    fi

    # Pattern files: private, one non-empty line each.
    PT=$(mktemp -d); TMP="$PT"; TEST_PUBKEY="$PK_A"
    write_pubkey_patterns
    assert_match "pattern file mode 600" '^-rw-------' "$(ls -l "$PK_PATTERN")"
    assert_match "quoted pattern file mode 600" '^-rw-------' "$(ls -l "$PK_QUOTED_PATTERN")"
    assert_eq    "pattern file content" "$PK_A" "$(cat "$PK_PATTERN")"
    assert_eq    "quoted pattern file content" "\"$PK_A\"" "$(cat "$PK_QUOTED_PATTERN")"
    printf '{"x":"%s"}' "$PK_B" >"$PT/other.json"
    file_has_pattern "$PK_PATTERN" "$PT/other.json"; assert_eq "grep: absent → 1" "1" "$?"
    printf '{"x":"%s"}' "$PK_A" >"$PT/hit.json"
    file_has_pattern "$PK_QUOTED_PATTERN" "$PT/hit.json"; assert_eq "grep: present → 0" "0" "$?"
    file_has_pattern "$PT/no-such.pat" "$PT/hit.json" 2>/dev/null
    assert_eq "grep: error is 2, not 'absent'" "2" "$?"
    rm -rf "$PT"

    # curl config: quotes and backslashes cannot end the url string.
    assert_eq "curl config: plain" 'url = "http://h/api/nodes/ab"' "$(curl_url_config 'http://h/api/nodes/ab')"
    assert_eq "curl config: escaped" 'url = "a\"b\\c"' "$(curl_url_config 'a"b\c')"
fi

# ----- teardown exit status ----------------------------------------------------
# Drives the script's own install_teardown_traps in a child bash with the side
# effects stubbed. The run must always tear down, and an interrupted run must
# never exit 0: before the fix, SIGTERM tore down and then exited as a pass.
TD_DIR=$(mktemp -d)
# MODE: exit | term | int | pipe | exit-sig-in-teardown | stderr-write |
# stdout-write. exit-sig-in-teardown signals the run while teardown is
# restoring the target; teardown must still finish. The *-write modes make one
# plain write from the shell itself — combined with a broken stream (below)
# that is how the shell, rather than a child, meets SIGPIPE.
# Anything after the three arguments is a wrapper command (a stream breaker).
teardown_case() {  # MODE FAILS NODE_VISIBLE_RC [WRAPPER...] → prints exit status
    local mode=$1 fails=$2 visible=$3; shift 3
    : >"$TD_DIR/calls"
    "$@" bash -c '
        calls="$2/calls"; visible_rc=$3; fails=$4; mode=$5
        . "$1"
        remove_from_blacklist() { echo remove >>"$calls"; }
        # A child started during teardown must still die on SIGINT, or a hung ssh
        # could not be interrupted. It signals itself; "survived" means the
        # disposition was inherited as ignored (trap -p cannot show that).
        restart_target() { echo "child-int:$(sh -c "kill -INT \$\$; echo survived" 2>/dev/null)" >>"$calls"; }
        wait_for_stats() {
            if [ "$mode" = exit-sig-in-teardown ]; then kill -TERM $$; kill -INT $$; kill -PIPE $$; fi
        }
        node_visible() { echo visible-checked >>"$calls"; return "$visible_rc"; }
        TMP=$(mktemp -d); TEST_PUBKEY=synthetic; TEARDOWN_DONE=0
        install_teardown_traps
        case "$mode" in
            term) kill -TERM $$; sleep 5; exit 0 ;;
            int)  kill -INT  $$; sleep 5; exit 0 ;;
            pipe) kill -PIPE $$; sleep 5; exit 0 ;;
            hup)  kill -HUP  $$; sleep 5; exit 0 ;;
            stderr-write) echo "diagnostic" >&2; exit "$fails" ;;
            stdout-write) echo "progress";       exit "$fails" ;;
            exit|exit-sig-in-teardown) exit "$fails" ;;
        esac
    ' _ "$BLACKLIST_SH" "$TD_DIR" "$visible" "$fails" "$mode" >/dev/null 2>&1
    echo $?
}
tore_down() { [ "$(grep -c remove "$TD_DIR/calls")" = 1 ] && grep -q visible-checked "$TD_DIR/calls"; }
assert_eq "clean run exits 0"              "0"   "$(teardown_case exit 0 0)"
assert_eq "clean run tore down once"       "1"   "$(grep -c remove "$TD_DIR/calls")"
assert_eq "two failures exit 2"            "2"   "$(teardown_case exit 2 0)"
assert_eq "teardown failure adds 1"        "3"   "$(teardown_case exit 2 1)"
assert_eq "SIGTERM mid-run exits 143"      "143" "$(teardown_case term 0 0)"
assert_eq "SIGTERM still tore down (once)" "1"   "$(grep -c remove "$TD_DIR/calls")"
assert_eq "SIGTERM + failed teardown"      "144" "$(teardown_case term 0 1)"
assert_eq "SIGINT mid-run exits 130"       "130" "$(teardown_case int 0 0)"
assert_eq "SIGINT still tore down (once)"  "1"   "$(grep -c remove "$TD_DIR/calls")"
assert_eq "SIGINT + failed teardown"       "131" "$(teardown_case int 0 1)"
assert_eq   "SIGHUP mid-run exits 129"     "129" "$(teardown_case hup 0 0)"
assert_true "SIGHUP mid-run tore down"     tore_down
assert_eq   "SIGHUP + failed teardown"     "130" "$(teardown_case hup 0 1)"
assert_eq "signal during teardown: status kept" "2" "$(teardown_case exit-sig-in-teardown 2 0)"
assert_eq "signal during teardown: teardown finished" "1" "$(grep -c visible-checked "$TD_DIR/calls")"
assert_eq "teardown children still die on SIGINT" "child-int:" "$(grep child-int "$TD_DIR/calls")"

# SIGPIPE (issue #83). kill -PIPE is the plain case; the stream breaker makes
# the shell's own write raise it for real. A shell killed by SIGPIPE also exits
# 141, so every case also checks that teardown actually ran.
assert_eq   "SIGPIPE mid-run exits 141"      "141" "$(teardown_case pipe 0 0)"
assert_true "SIGPIPE mid-run tore down"      tore_down
assert_eq   "SIGPIPE + failed teardown"      "142" "$(teardown_case pipe 0 1)"
if command -v perl >/dev/null 2>&1; then
    assert_eq   "broken stderr, shell writes: 141" "141" "$(teardown_case stderr-write 0 0 perl -e "$PERL_BREAK" 2)"
    assert_true "broken stderr, shell writes: tore down" tore_down
    assert_eq   "broken stdout, shell writes: 141" "141" "$(teardown_case stdout-write 2 0 perl -e "$PERL_BREAK" 1)"
    assert_true "broken stdout, shell writes: tore down" tore_down
    assert_eq   "broken stderr, failed teardown: 142" "142" "$(teardown_case stderr-write 0 1 perl -e "$PERL_BREAK" 2)"
    # The INT/TERM notice in teardown goes to the broken stderr. Before #83
    # that SIGPIPE killed the shell mid-restore.
    assert_eq   "broken stderr, signal in teardown: status kept" "2" \
                "$(teardown_case exit-sig-in-teardown 2 0 perl -e "$PERL_BREAK" 2)"
    assert_true "broken stderr, signal in teardown: teardown finished" tore_down
    # say on a dead stdout must not leave text for the next subshell to
    # replay (bash 3.2 keeps it buffered; bash 5 purges it).
    replay=$(perl -e "$PERL_IGNORE" 1 bash -c '. "$1"; say "STALE-MARK"; x=$(printf clean); printf "%s" "$x" >&3' _ "$BLACKLIST_SH" 3>&1 2>/dev/null)
    assert_eq   "dead stdout: nothing replayed into a capture" "clean" "$replay"
    # Started with SIGPIPE ignored: not trappable, so writes fail with EPIPE and
    # the run keeps its real status — never a pass it did not earn.
    assert_eq   "SIGPIPE ignored on entry: status kept" "2" "$(teardown_case stderr-write 2 0 perl -e "$PERL_IGNORE" 2)"
    assert_true "SIGPIPE ignored on entry: tore down" tore_down
elif [ -n "${CI:-}" ]; then
    FAIL=$((FAIL + 1)); echo "FAIL: perl not on PATH in CI — the SIGPIPE stream group cannot run" >&2
else
    echo "SKIP: perl not on PATH — skipping the SIGPIPE stream group" >&2
fi
rm -rf "$TD_DIR"

echo "test-blacklist-sql.sh: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
