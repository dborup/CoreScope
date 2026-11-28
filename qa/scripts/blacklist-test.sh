#!/usr/bin/env bash
# blacklist-test.sh — verify nodeBlacklist hides a pubkey from API surface
# while retaining its packets in the DB. Implements QA plan §10.1 + §10.2.
#
# Usage:
#   blacklist-test.sh BASELINE_URL TARGET_URL
#
# BASELINE_URL is currently unused for assertions but kept as a positional
# arg for parity with other qa-suite scripts (always called with two URLs).
#
# Required env (target host control + test data):
#   TEST_NODE_PUBKEY      — hex pubkey of a real, currently-visible node on TARGET_URL
#   TARGET_SSH_HOST       — e.g. runner@example
#   TARGET_SSH_KEY        — path to ssh private key (default: /root/.ssh/id_ed25519)
#   TARGET_CONFIG_PATH    — absolute path to config.json on the target
#   TARGET_CONTAINER      — docker container name on the target
# Optional env:
#   TARGET_CONTAINER_DB_PATH — sqlite db path INSIDE TARGET_CONTAINER. Used only
#                              by the container runner (docker exec … sqlite3).
#   TARGET_HOST_DB_PATH      — sqlite db path on TARGET_SSH_HOST's own
#                              filesystem. Used only by the host runner.
#                              §10.2 needs at least one of the two; they may
#                              differ, and neither is ever tried in the other
#                              environment.
#   CURL_TIMEOUT          — per-request curl timeout, seconds (default 60)
#   RESTART_WAIT_S        — seconds after a restart during which a new
#                           /api/stats poll may still start (default 120). A poll
#                           in flight at the deadline runs to completion, so the
#                           wait can last up to RESTART_WAIT_S + CURL_TIMEOUT + 3.
#
# Removed env — the run refuses to start (exit 2) if either is set, rather than
# silently doing something other than what the operator asked for:
#   TARGET_DB_PATH        — named one path for two filesystems. The host runner
#                           used the container's path, and the host's sqlite3
#                           could create an empty database there.
#   ADMIN_API_TOKEN       — queried /api/admin/transmissions, which CoreScope
#                           has never had; every run fell through to SQLite.
#                           §10.2 is SQLite-only.
#
# Process arguments: the pubkey, the SQL and every URL stay out of argv on both
# ends. The config edit reads the pubkey from ssh stdin, curl reads its URL from
# a config on stdin (-K -), grep reads its pattern from a mode-600 file under
# the run's mode-700 temp dir, and SQL crosses to sqlite3 on stdin.
#
# Distinguishes:
#   ssh-failed     → cannot reach/control target
#   restart-stuck  → no /api/stats poll started within RESTART_WAIT_S returned 200
#   hide-failed    → blacklisted pubkey still surfaced via API (§10.1 fail)
#   retain-failed  → no transmissions.from_pubkey rows (ADVERTs) for the
#                    blacklisted pubkey in the DB (§10.2 fail), or the
#                    §10.2 probe could not run at all — no db path configured,
#                    no sqlite3 able to bind a parameter, or the configured path
#                    is not a non-empty file in its runner's environment. The
#                    message names what is needed; there is no fallback to
#                    interpolated SQL, and no path is tried in the wrong place.
#   teardown-failed→ post-test removal did not restore listing
#
# Exit code = number of failures (0 = pass). An interrupted run still tears
# down, then exits 130 (SIGINT), 143 (SIGTERM), 129 (SIGHUP) or 141 (SIGPIPE),
# plus 1 if teardown failed.
#
# SIGPIPE: the script's own writes go to stdout/stderr, so if either is a pipe
# whose reader has gone, the shell gets SIGPIPE. It is handled like INT/TERM:
# teardown, then 141, plus 1 if teardown failed. Unhandled, bash would still
# run the EXIT trap but then re-raise the signal, so the status would be 141
# whatever teardown found, and teardown's own next write to the dead stream
# would raise SIGPIPE again and cut the restore short. Inside teardown SIGPIPE
# is ignored, so a lost output channel cannot do that. Outside teardown child
# processes keep the default disposition, and one killed by SIGPIPE fails its
# step, which is classified as usual; inside it they inherit the ignore and
# see EPIPE instead. A shell started with SIGPIPE already ignored cannot
# trap it; its writes then fail with EPIPE and the run continues to its normal
# exit status. Either way a dead stream is sent to /dev/null (see say/warn),
# never replayed.
#
# A further INT/TERM/HUP does not abort teardown: it stops only the step in
# progress. An interrupted ssh step fails and is reported as teardown-failed;
# an interrupted /api/stats poll only counts as one failed poll, so the wait
# goes on until RESTART_WAIT_S and teardown can still succeed.
# PUBLIC repo: zero PII — no real pubkeys, IPs, or hostnames as defaults.
#
# Structure: helpers live at top level and the imperative body lives in main(),
# so test-blacklist-sql.sh can source this file and exercise individual helpers
# without running the suite. Same idiom as scripts/staging/disk-monitor.sh.

set -uo pipefail

SSH_OPTS=()  # populated by main() from TARGET_SSH_KEY
# Callers build the remote command line here on purpose, quoting every value
# with printf -v %q; nothing else is meant to reach the remote shell.
# shellcheck disable=SC2029
ssh_t() { ssh "${SSH_OPTS[@]}" "$TARGET_SSH_HOST" "$@"; }

# Output. All of the script's own messages go through say (stdout) and warn
# (stderr), never a bare echo, because of how bash 3.2 — still macOS's
# /bin/bash — handles a write that fails: it keeps the unwritten text in its
# stdout buffer, and the next subshell flushes that text into ITS output. The
# next subshell might be a $(...) capture, or the group that pipes the config
# edit into ssh, whose remote bash would then run the leftover lines as
# commands. (bash 5 purges the buffer; the failure mode is 3.2's.) So a stream
# found dead is pointed at /dev/null for the rest of the run and the leftover
# buffer is flushed there at once; the run's exit status carries the result.
# A write can only fail this way once SIGPIPE is not killing the shell: inside
# teardown, where it is ignored, or when the shell started with it ignored.
stream_dead() {
  if [[ "$1" == 1 ]]; then exec 1>/dev/null; else exec 2>/dev/null; fi
  echo -n '' >/dev/null  # flush 3.2's leftover buffer into /dev/null
}
say()  { printf '%s\n' "$*" 2>/dev/null || stream_dead 1; }
warn() { printf '%s\n' "$*" >&2 2>/dev/null || stream_dead 2; }

# -----------------------------------------------------------------------------
# Teardown — MANDATORY in all exit paths.
# -----------------------------------------------------------------------------
teardown() {
  local rc=$?
  # Signal traps pass the conventional 128+signo. Without it $? is the status
  # of whatever ran before the signal — usually 0 — and an aborted run would
  # exit as a pass.
  if [[ -n "${1:-}" ]]; then rc=$1; fi
  if [[ "$TEARDOWN_DONE" == "1" ]]; then rm -rf "$TMP"; exit "$rc"; fi
  TEARDOWN_DONE=1
  # A closed stdout/stderr must not end the restore: every message below, and
  # the INT/TERM notice, would otherwise raise SIGPIPE again with no handler
  # left to finish the job. Ignored rather than handled, so a write just fails
  # (EPIPE) and say/warn retire the dead stream.
  trap '' PIPE
  # A second Ctrl-C/TERM must not abort the restore, or the node would stay
  # blacklisted with no warning. Note the signal rather than ignoring it: an
  # ignored disposition is inherited by ssh/curl, so a hung step could no longer
  # be interrupted. With a handler, a terminal Ctrl-C still stops the running
  # step; that step fails and teardown reports teardown-failed.
  trap 'warn "  (signal received — teardown continues restoring the target)"' INT TERM HUP
  say "=== teardown: removing $TEST_PUBKEY from nodeBlacklist ==="
  if remove_from_blacklist && restart_target && wait_for_stats; then
    if node_visible; then
      say "  ✅ teardown ok — node returned to listings"
    else
      say "  ❌ teardown-failed: node still hidden after removal"
      rc=$((rc + 1))
    fi
  else
    say "  ❌ teardown-failed: could not restore config / restart / stats"
    rc=$((rc + 1))
  fi
  rm -rf "$TMP"
  exit "$rc"
}

install_teardown_traps() {
  trap teardown EXIT
  trap 'teardown 130' INT
  trap 'teardown 143' TERM
  trap 'teardown 129' HUP
  trap 'teardown 141' PIPE
}

# -----------------------------------------------------------------------------
# Helpers
# -----------------------------------------------------------------------------
# curl config line for a URL. main() rejects URLs with quotes, backslashes,
# whitespace or control characters, so the escaping here is defence in depth.
curl_url_config() {
  local u="$1"
  u=${u//\\/\\\\}
  u=${u//\"/\\\"}
  printf 'url = "%s"\n' "$u"
}

# The URL reaches curl as a config on stdin (-K -), never as an argument:
# /api/nodes/<pubkey> would otherwise put the pubkey in curl's argv.
fetch_code() {
  local url="$1" out="$2"
  curl_url_config "$url" | curl -s -m "$CURL_TIMEOUT" -K - -o "$out" -w "%{http_code}" 2>/dev/null || echo "000"
}

# Pattern files for grep -f, written once by main() under the mode-700 $TMP.
# Each holds exactly one non-empty line (the pubkey is validated non-empty hex),
# which matters: an empty pattern line would make grep -F match everything.
PK_PATTERN=""         # bare pubkey
PK_QUOTED_PATTERN=""  # the pubkey as a JSON string, "…"
write_pubkey_patterns() {
  PK_PATTERN="$TMP/pubkey.pat"
  PK_QUOTED_PATTERN="$TMP/pubkey-quoted.pat"
  ( umask 077
    printf '%s\n' "$TEST_PUBKEY" >"$PK_PATTERN"
    printf '"%s"\n' "$TEST_PUBKEY" >"$PK_QUOTED_PATTERN" )
}

# Whole-line variant, for lists of extracted values (one per line).
file_has_line() {
  local pattern_file="$1" file="$2"
  grep -qxF -f "$pattern_file" -- "$file"
}

# grep FILE for a pattern file. Status is grep's: 0 found, 1 not found, 2 error.
# Callers must not read 2 as "not found" — for a hide check that is a pass.
file_has_pattern() {
  local pattern_file="$1" file="$2"
  grep -qF -f "$pattern_file" -- "$file"
}

# §10.1 topology. The route is /api/analytics/topology (cmd/server/routes.go);
# /api/topology does not exist and the SPA fallback answers it 200 with HTML,
# which used to pass as "clean". Only a 200 whose body has the TopologyResponse
# shape can pass. The pubkeys it carries are the fields the server's
# filterBlacklistedFromTopology filters: topRepeaters[].pubkey,
# topPairs[].pubkeyA/pubkeyB, bestPathList[].pubkey, multiObsNodes[].pubkey and
# perObserverReach{}.rings[].nodes[].pubkey. Those are extracted, lower-cased,
# one per line, and compared whole-line with the canonical pubkey — not a text
# grep over the body. Arrays the server filtered empty come back as null.
# A shape mismatch makes jq exit non-zero ("not the expected topology JSON").
# jq runs with -n and reads the body itself: exactly one JSON document, so an
# empty or blank body (which plain `jq FILE` accepts silently, printing
# nothing — "clean") or two concatenated documents fail too.
# shellcheck disable=SC2016  # a jq program: $r and $k are jq variables
TOPOLOGY_PUBKEYS_JQ='
  def list(k): .[k] // [] | if type == "array" then . else error("\(k) is not an array") end;
  [inputs] | if length == 1 then .[0] else error("expected one JSON document, got \(length)") end
  | if type != "object" then error("not an object") else . end
  | . as $r
  | if (["uniqueNodes","topRepeaters","topPairs","bestPathList","multiObsNodes","perObserverReach"]
        | all(. as $k | $r | has($k))) and ((.uniqueNodes | type) == "number")
    then . else error("missing topology fields") end
  | [ (list("topRepeaters")[] | .pubkey),
      (list("topPairs")[] | .pubkeyA, .pubkeyB),
      (list("bestPathList")[] | .pubkey),
      (list("multiObsNodes")[] | .pubkey),
      (.perObserverReach // {}
        | if type == "object" then . else error("perObserverReach is not an object") end
        | .[] | (.rings // [])[] | (.nodes // [])[] | .pubkey) ]
  | .[] | select(type == "string") | ascii_downcase'

# Fetch the topology into $TMP/topo.json, setting TOPO_CODE. The analytics
# recomputer answers 503 while it warms up after a restart; that is waited out
# for up to RESTART_WAIT_S. Any other code is final.
fetch_topology() {
  local deadline=$(( $(date +%s) + RESTART_WAIT_S ))
  while :; do
    TOPO_CODE=$(fetch_code "$TARGET_URL/api/analytics/topology" "$TMP/topo.json")
    [[ "$TOPO_CODE" == "503" ]] || return 0
    (( $(date +%s) < deadline )) || return 0
    sleep 3
  done
}

wait_for_stats() {
  local deadline code
  say "  polling $TARGET_URL/api/stats; no new poll starts after ${RESTART_WAIT_S}s ..."
  deadline=$(( $(date +%s) + RESTART_WAIT_S ))
  while (( $(date +%s) < deadline )); do
    code=$(fetch_code "$TARGET_URL/api/stats" "$TMP/stats.json")
    if [[ "$code" == "200" ]]; then say "  stats OK"; return 0; fi
    sleep 3
  done
  say "  ❌ restart-stuck: no /api/stats poll started within ${RESTART_WAIT_S}s returned 200"
  return 1
}

restart_target() {
  say "  restarting container $TARGET_CONTAINER ..."
  # TARGET_CONTAINER is validated above; still quote defensively.
  local q_container
  printf -v q_container '%q' "$TARGET_CONTAINER"
  if ! ssh_t "docker restart $q_container" </dev/null >/dev/null; then
    say "  ❌ ssh-failed: docker restart failed"
    return 1
  fi
  return 0
}

# Run the remote config script on the target. MODE is check | add | remove.
# The pubkey travels on ssh's stdin, in the line right after the remote
# script's first command, which reads it: bash -s reads its script from stdin
# without reading ahead, so `read` gets exactly that line (POSIX requires this
# of a shell reading commands from stdin). From there it reaches jq/python3
# through the environment. It is therefore in neither the local ssh argv nor
# any remote argv; only the config path and the constant mode are on the
# command line. Returns the remote status; for check: 0 = not blacklisted,
# 10 = already blacklisted, anything else = could not tell. check compares the
# way the server does (cmd/server/config.go buildBlacklistSet): trimmed and
# lower-cased. A missing or null nodeBlacklist means none; a non-array one, or
# a config that is not JSON, is "could not tell" — never "not blacklisted".
#
# add appends and remove drops only the exact canonical value, so the rest of
# nodeBlacklist — order, duplicates, other spellings — is left as it was. add
# only ever runs after check has shown the value absent, which is what makes
# teardown's remove an exact undo.
remote_config() {
  local q_cfg q_mode
  printf -v q_cfg '%q' "$TARGET_CONFIG_PATH"
  printf -v q_mode '%q' "$1"
  {
    printf 'IFS= read -r PK || exit 3\n%s\n' "$TEST_PUBKEY"
    cat <<'REMOTE'
set -euo pipefail
case "$PK" in ""|*[!0-9a-fA-F]*) echo "blacklist-test: pubkey on stdin is not hex" >&2; exit 3 ;; esac
case "$MODE" in check|add|remove) ;; *) echo "blacklist-test: bad mode" >&2; exit 3 ;; esac
export PK
if [ "$MODE" = check ]; then
  if command -v jq >/dev/null; then
    if jq -e '(.nodeBlacklist // [])
              | if type == "array" then . else error("nodeBlacklist is not an array") end
              | any(.[]; type == "string" and (ascii_downcase | gsub("^\\s+|\\s+$"; "")) == (env.PK | ascii_downcase)) | not' \
          "$CFG" >/dev/null; then
      exit 0
    else
      rc=$?; [ "$rc" = 1 ] && exit 10; exit 4
    fi
  fi
  python3 - "$CFG" <<'PY'
import json, os, sys
pk = os.environ["PK"].lower()
try:
    with open(sys.argv[1]) as f: bl = json.load(f).get("nodeBlacklist") or []
except Exception:
    sys.exit(4)
if not isinstance(bl, list): sys.exit(4)
sys.exit(10 if any(isinstance(x, str) and x.strip().lower() == pk for x in bl) else 0)
PY
  exit 0
fi
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT
if command -v jq >/dev/null; then
  if [ "$MODE" = "add" ]; then
    jq '.nodeBlacklist = ((.nodeBlacklist // []) | if any(.[]; . == env.PK) then . else . + [env.PK] end)' "$CFG" > "$TMP"
  else
    jq '.nodeBlacklist = ((.nodeBlacklist // []) - [env.PK])' "$CFG" > "$TMP"
  fi
else
  python3 - "$CFG" "$MODE" "$TMP" <<'PY'
import json, os, sys
cfg, mode, out = sys.argv[1:]
pk = os.environ["PK"]
with open(cfg) as f: d = json.load(f)
bl = list(d.get("nodeBlacklist") or [])
if mode == "add":
    if pk not in bl: bl.append(pk)
else:
    bl = [x for x in bl if x != pk]
d["nodeBlacklist"] = bl
with open(out, "w") as f: json.dump(d, f, indent=2)
PY
fi
# Preserve mode and ownership; mv across same FS is atomic.
chmod --reference="$CFG" "$TMP" 2>/dev/null || true
chown --reference="$CFG" "$TMP" 2>/dev/null || true
mv "$TMP" "$CFG"
trap - EXIT
REMOTE
  } | ssh_t "CFG=$q_cfg MODE=$q_mode bash -s"
}

set_blacklist_state() {
  local mode="$1"  # add | remove
  if ! remote_config "$mode"; then
    say "  ❌ ssh-failed: could not edit $TARGET_CONFIG_PATH ($mode)"
    return 1
  fi
  return 0
}

add_to_blacklist()      { set_blacklist_state add; }
remove_from_blacklist() { set_blacklist_state remove; }

node_visible() {
  # Returns 0 if the pubkey is currently visible via API.
  local code
  code=$(fetch_code "$TARGET_URL/api/nodes/$TEST_PUBKEY" "$TMP/node.json")
  if [[ "$code" == "200" ]]; then return 0; fi
  fetch_code "$TARGET_URL/api/nodes?limit=10000" "$TMP/nodes.json" >/dev/null
  if file_has_pattern "$PK_QUOTED_PATTERN" "$TMP/nodes.json" 2>/dev/null; then
    return 0
  fi
  return 1
}

# -----------------------------------------------------------------------------
# §10.2 DB probe — bind the pubkey, do not interpolate it (issue #1977)
# -----------------------------------------------------------------------------
# Batch flags, all in service of "the count is parseable and errors are visible":
#   -bail            stop at the first SQL error instead of running on
#   -init /dev/null  ignore the operator's ~/.sqliterc — a stray .mode there
#                    would make the count unparseable
#   -noheader -list  stdout is exactly the number, nothing else
#   -readonly        never create or modify the database. A path that does not
#                    exist fails "unable to open database" instead of leaving an
#                    empty file behind. db_path_ok checks the path first; this is
#                    the second, independent guard.
#                    CoreScope's database is in WAL mode: a read-only open
#                    needs the -wal/-shm files a running app keeps; §10.2
#                    runs after /api/stats has shown the restarted app is up.
SQLITE_ARGS=(-batch -bail -init /dev/null -noheader -list -readonly)
# Round-trip probe token. The value is arbitrary; it only has to come back intact.
SQLITE_PROBE_TOKEN="corescope-probe-ok"
SQLITE_RUNNER=""   # "container" | "host", set by resolve_sqlite_runner
RETAIN_COUNT=""    # set by read_retain_count

# Hex-encode a value for embedding in SQL as a blob literal.
#
# Why hex rather than quoting: the output alphabet is [0-9a-f], so no byte the
# caller passes can terminate a string literal or add a dot-command argument.
# That holds for arbitrary input, which is the point — the SQL layer stops
# depending on main()'s hex gate in order to be safe.
#
# `od -v` is load-bearing: without it od collapses runs of identical lines to
# '*' and long repetitive values encode wrongly.
sql_hex_literal() {
  printf "x'%s'" "$(printf '%s' "$1" | od -An -v -tx1 | tr -d ' \n')"
}

# SQL for the §10.2 count, fed to sqlite3 on stdin. The SELECT text is a
# constant; the pubkey arrives as a bound parameter.
#
# Note the nested cast rather than `.parameter set :pubkey '<value>'`:
# dot-command arguments are split on whitespace, so a value containing a space
# (e.g. "' OR 1=1 --") makes sqlite3 print the .parameter help to STDOUT, exit
# 0, and leave :pubkey unbound. COUNT(*) then returns 0 — which reads exactly
# like a passing security fix. -bail does not catch it either.
#
# The column is transmissions.from_pubkey (cmd/ingestor/db.go CREATE TABLE and
# the from_pubkey_v1 migration; asserted by internal/dbschema). The ingestor
# fills it only for ADVERTs, with hex.EncodeToString output — lowercase. The
# script's hex gate and the server's nodeBlacklist both accept any case, so the
# bound value is lowercased inside SQL; the parameter itself is still bound.
# There is no from_node column: querying it errors on every real database.
transmission_count_sql() {
  printf '.parameter init\n'
  printf '.parameter set :pubkey "cast(%s as text)"\n' "$(sql_hex_literal "$1")"
  printf 'SELECT COUNT(*) FROM transmissions WHERE from_pubkey = lower(:pubkey);\n'
}

# Capability probe: bind a known value and read it back. A version number only
# implies that .parameter works; binding something and getting it back proves it
# on the binary actually in front of us, which is the operator's, not ours.
sqlite_probe_sql() {
  printf '.parameter init\n'
  printf '.parameter set :probe "cast(%s as text)"\n' "$(sql_hex_literal "$SQLITE_PROBE_TOKEN")"
  printf 'SELECT :probe;\n'
}

# Probe one runner. Stderr is collected rather than discarded, but only printed
# if no runner qualifies.
probe_runner() {
  local runner="$1" probe out q_container
  probe=$(sqlite_probe_sql)
  printf -v q_container '%q' "$TARGET_CONTAINER"
  case "$runner" in
    container) out=$(ssh_t "docker exec -i $q_container sqlite3 ${SQLITE_ARGS[*]} :memory:" \
                 <<<"$probe" 2>>"$TMP/sqlite-probe.err") ;;
    host)      out=$(ssh_t "sqlite3 ${SQLITE_ARGS[*]} :memory:" <<<"$probe" 2>>"$TMP/sqlite-probe.err") ;;
    *)         return 1 ;;
  esac
  [[ "$out" == "$SQLITE_PROBE_TOKEN" ]]
}

# Find a sqlite3 that can bind a parameter — in the container first, then on the
# host. Sets SQLITE_RUNNER; returns 1 if neither qualifies. There is deliberately
# no interpolating fallback: that would leave the vulnerable path in place under
# a nicer name.
#
# A runner is only a candidate if its own path is configured: the container is
# tried only with TARGET_CONTAINER_DB_PATH set, the host only with
# TARGET_HOST_DB_PATH. The container miss is the known-normal case — the app
# image has no sqlite3 (pure-Go driver, no CGO; Dockerfile:15) — so it falls
# back to the host when a host path is configured, and is not surfaced unless
# nothing qualifies.
resolve_sqlite_runner() {
  SQLITE_RUNNER=""
  if [[ -n "$TARGET_CONTAINER_DB_PATH" ]] && probe_runner container; then
    SQLITE_RUNNER="container"; return 0
  fi
  if [[ -n "$TARGET_HOST_DB_PATH" ]] && probe_runner host; then
    SQLITE_RUNNER="host"; return 0
  fi
  return 1
}

# The db path for the resolved runner, and the env var it came from, for
# messages. Each runner has exactly one path; there is no fallback from one to
# the other.
runner_db_path() {
  case "$SQLITE_RUNNER" in
    container) printf '%s' "$TARGET_CONTAINER_DB_PATH" ;;
    host)      printf '%s' "$TARGET_HOST_DB_PATH" ;;
    *)         return 1 ;;
  esac
}
runner_db_var() {
  case "$SQLITE_RUNNER" in
    container) printf 'TARGET_CONTAINER_DB_PATH' ;;
    host)      printf 'TARGET_HOST_DB_PATH' ;;
    *)         return 1 ;;
  esac
}

# Is the runner's db path a non-empty regular file in the runner's own
# environment? Checked before any query, so a wrong path is named as such rather
# than surfacing as a sqlite error — and so sqlite3 is never pointed at it.
# Stdin is closed: nothing crosses for a test.
db_path_ok() {
  local q_container q_path
  printf -v q_container '%q' "$TARGET_CONTAINER"
  case "$SQLITE_RUNNER" in
    container) printf -v q_path '%q' "$TARGET_CONTAINER_DB_PATH"
               ssh_t "docker exec $q_container sh -c 'test -f \"\$1\" && test -s \"\$1\"' sh $q_path" </dev/null ;;
    host)      printf -v q_path '%q' "$TARGET_HOST_DB_PATH"
               ssh_t "test -f $q_path && test -s $q_path" </dev/null ;;
    *)         return 1 ;;
  esac
}

# Run SQL from stdin against the resolved runner's own db path. Stderr is left
# alone so the caller can capture it, and the exit status is sqlite3's.
# The SQL crosses on stdin, so only the container name and db path still need
# printf %q for the remote shell. docker exec needs -i to attach stdin.
run_sqlite() {
  local q_container q_path
  printf -v q_container '%q' "$TARGET_CONTAINER"
  case "$SQLITE_RUNNER" in
    container) printf -v q_path '%q' "$TARGET_CONTAINER_DB_PATH"
               ssh_t "docker exec -i $q_container sqlite3 ${SQLITE_ARGS[*]} $q_path" ;;
    host)      printf -v q_path '%q' "$TARGET_HOST_DB_PATH"
               ssh_t "sqlite3 ${SQLITE_ARGS[*]} $q_path" ;;
    *)         warn "run_sqlite: no runner resolved"; return 127 ;;
  esac
}

# Read the retained-transmission count into RETAIN_COUNT. Prints a classified
# "retain-failed" line and returns 1 on failure, so §10.2 has exactly one place
# that increments $fails.
#
# SQLite is the only source. CoreScope has no HTTP endpoint that counts a
# blacklisted node's transmissions, and adding one for a QA script would be a
# production change; the ADMIN_API_TOKEN path this replaced called an endpoint
# that never existed.
read_retain_count() {
  RETAIN_COUNT=""
  if [[ -z "$TARGET_CONTAINER_DB_PATH" && -z "$TARGET_HOST_DB_PATH" ]]; then
    say "  ❌ retain-failed: no db path — set TARGET_CONTAINER_DB_PATH and/or TARGET_HOST_DB_PATH"
    return 1
  fi
  if ! resolve_sqlite_runner; then
    say "  ❌ retain-failed: no sqlite3 able to bind a parameter on the target"
    [[ -n "$TARGET_CONTAINER_DB_PATH" ]] && say "     tried: docker exec -i $TARGET_CONTAINER sqlite3 (TARGET_CONTAINER_DB_PATH set)"
    [[ -n "$TARGET_HOST_DB_PATH" ]]      && say "     tried: sqlite3 on $TARGET_SSH_HOST (TARGET_HOST_DB_PATH set)"
    say "     need:  the sqlite3 CLI reachable over ssh, supporting '.parameter set' and -readonly"
    cat "$TMP/sqlite-probe.err" >&2
    return 1
  fi
  say "  sqlite3 runner: $SQLITE_RUNNER ($(runner_db_var))"
  local path_rc=0
  db_path_ok || path_rc=$?
  if (( path_rc == 255 )); then
    say "  ❌ retain-failed: ssh failed while checking $(runner_db_var) (exit 255)"
    return 1
  elif (( path_rc != 0 )); then
    say "  ❌ retain-failed: $(runner_db_var)=$(runner_db_path) is not a non-empty file in the $SQLITE_RUNNER (check exit $path_rc)"
    return 1
  fi
  if ! RETAIN_COUNT=$(run_sqlite <<<"$(transmission_count_sql "$TEST_PUBKEY")" 2>"$TMP/sqlite.err"); then
    say "  ❌ retain-failed: sqlite3 query failed via $SQLITE_RUNNER"
    cat "$TMP/sqlite.err" >&2
    RETAIN_COUNT=""
    return 1
  fi
  return 0
}

# -----------------------------------------------------------------------------
# main
# -----------------------------------------------------------------------------
main() {
  BASELINE_URL="${1:-}"
  TARGET_URL="${2:-}"
  if [[ -z "$BASELINE_URL" || -z "$TARGET_URL" ]]; then
    warn "usage: $0 BASELINE_URL TARGET_URL  (TEST_NODE_PUBKEY+TARGET_* via env)"
    exit 2
  fi

  TEST_PUBKEY="${TEST_NODE_PUBKEY:-}"
  TARGET_SSH_HOST="${TARGET_SSH_HOST:-}"
  TARGET_SSH_KEY="${TARGET_SSH_KEY:-/root/.ssh/id_ed25519}"
  TARGET_CONFIG_PATH="${TARGET_CONFIG_PATH:-}"
  TARGET_CONTAINER="${TARGET_CONTAINER:-}"
  TARGET_CONTAINER_DB_PATH="${TARGET_CONTAINER_DB_PATH:-}"
  TARGET_HOST_DB_PATH="${TARGET_HOST_DB_PATH:-}"

  if [[ -z "$TEST_PUBKEY" || -z "$TARGET_SSH_HOST" || -z "$TARGET_CONFIG_PATH" || -z "$TARGET_CONTAINER" ]]; then
    warn "error: TEST_NODE_PUBKEY, TARGET_SSH_HOST, TARGET_CONFIG_PATH, TARGET_CONTAINER are required"
    exit 2
  fi

  # Removed variables fail the run before anything touches the target. Ignoring
  # them would run a different probe from the one the operator configured; the
  # values themselves are never printed.
  if [[ -n "${TARGET_DB_PATH:-}" ]]; then
    warn "error: TARGET_DB_PATH is no longer read — it named one path for two filesystems."
    warn "       Set TARGET_CONTAINER_DB_PATH (inside TARGET_CONTAINER) and/or"
    warn "       TARGET_HOST_DB_PATH (on TARGET_SSH_HOST)."
    exit 2
  fi
  if [[ -n "${ADMIN_API_TOKEN:-}" ]]; then
    warn "error: ADMIN_API_TOKEN is no longer read — /api/admin/transmissions never existed;"
    warn "       §10.2 is SQLite-only (TARGET_CONTAINER_DB_PATH / TARGET_HOST_DB_PATH)."
    exit 2
  fi

  # Hard input validation — these strings are interpolated into the remote shell.
  # §10.2's SQL binds TEST_PUBKEY as a parameter rather than interpolating it, so
  # for the SQL layer this gate is defence in depth rather than the only guard
  # (issue #1977). Keep it: redundant is not the same as wrong.
  # Pubkey must be hex (MeshCore pubkeys are hex-encoded ed25519 prefixes).
  if ! [[ "$TEST_PUBKEY" =~ ^[0-9a-fA-F]+$ ]]; then
    warn "error: TEST_NODE_PUBKEY must be hex (got: redacted)"
    exit 2
  fi
  # One canonical form from here on: lower case, as the ingestor stores it
  # (hex.EncodeToString) and the API returns it. The config entry, API paths,
  # pattern files and the SQLite binding all use this value. (tr reads the
  # value on stdin; only the character classes are arguments.)
  TEST_PUBKEY=$(printf '%s' "$TEST_PUBKEY" | tr 'A-F' 'a-f')
  # Container name must match docker's allowed chars: [a-zA-Z0-9][a-zA-Z0-9_.-]*
  if ! [[ "$TARGET_CONTAINER" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]]; then
    warn "error: TARGET_CONTAINER has illegal chars"
    exit 2
  fi
  # Config path must be an absolute, sane path (no spaces, quotes, $, ;, etc.).
  if ! [[ "$TARGET_CONFIG_PATH" =~ ^/[A-Za-z0-9_./-]+$ ]]; then
    warn "error: TARGET_CONFIG_PATH must be a sane absolute path"
    exit 2
  fi
  local var
  for var in TARGET_CONTAINER_DB_PATH TARGET_HOST_DB_PATH; do
    if [[ -n "${!var}" ]] && ! [[ "${!var}" =~ ^/[A-Za-z0-9_./-]+$ ]]; then
      warn "error: $var must be a sane absolute path"
      exit 2
    fi
  done
  # TARGET_URL reaches curl inside a quoted config line (-K -); keep it plain.
  local bad_url_re='[[:space:][:cntrl:]"\\]'
  if ! [[ "$TARGET_URL" =~ ^https?:// ]] || [[ "$TARGET_URL" =~ $bad_url_re ]]; then
    warn "error: TARGET_URL must be an http(s) URL without whitespace, quotes or backslashes"
    exit 2
  fi
  # The topology check parses JSON locally.
  if ! command -v jq >/dev/null 2>&1; then
    warn "error: jq is required on the machine running this script"
    exit 2
  fi

  CURL_TIMEOUT="${CURL_TIMEOUT:-60}"
  RESTART_WAIT_S="${RESTART_WAIT_S:-120}"

  # ServerAlive*: a dead connection mid-command fails after ~60s instead of
  # hanging; ConnectTimeout only bounds connection setup.
  SSH_OPTS=(-i "$TARGET_SSH_KEY" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 -o BatchMode=yes
            -o ServerAliveInterval=15 -o ServerAliveCountMax=4)

  # Is the pubkey already a privacy rule on the target? If so, refuse before
  # any side effect: teardown removes the pubkey, and would take the
  # operator's rule with it. Case-insensitive, like the server's blacklist.
  # Runs before the traps, so a refusal or a failure here tears nothing down,
  # and the message does not name the pubkey. It prints nothing on success
  # until the traps are in place, so a dead stdout still meets the trap.
  local pre_rc=0
  remote_config check || pre_rc=$?
  if (( pre_rc == 10 )); then
    say "  ❌ refused: TEST_NODE_PUBKEY is already in nodeBlacklist on the target."
    say "     Teardown would remove that existing rule, so nothing was changed."
    say "     Use a node that is not blacklisted."
    exit 2
  elif (( pre_rc != 0 )); then
    say "  ❌ ssh-failed: could not read nodeBlacklist from $TARGET_CONFIG_PATH (exit $pre_rc) — nothing was changed"
    exit 1
  fi

  # Everything the run writes locally is private to it: $TMP is mode 700, and
  # the pattern files and responses inside are mode 600.
  umask 077
  TMP=$(mktemp -d)
  fails=0
  TEARDOWN_DONE=0
  install_teardown_traps
  write_pubkey_patterns
  say "=== preflight: TEST_NODE_PUBKEY is not in the target's nodeBlacklist — ok ==="

  # ---------------------------------------------------------------------------
  # §10.1 — hide
  # ---------------------------------------------------------------------------
  say "=== §10.1 add $TEST_PUBKEY to nodeBlacklist ==="
  if ! add_to_blacklist; then fails=$((fails+1)); exit "$fails"; fi
  if ! restart_target;    then fails=$((fails+1)); exit "$fails"; fi
  if ! wait_for_stats;    then fails=$((fails+1)); exit "$fails"; fi

  detail_code=$(fetch_code "$TARGET_URL/api/nodes/$TEST_PUBKEY" "$TMP/detail.json")
  list_code=$(fetch_code "$TARGET_URL/api/nodes?limit=10000" "$TMP/list.json")
  # Hidden means BOTH: the detail endpoint 404s and the listing omits it. A
  # listing that could not be fetched or searched proves nothing, so it fails
  # rather than counting as "not in the list".
  in_list=unknown
  if [[ "$list_code" == "200" ]]; then
    file_has_pattern "$PK_QUOTED_PATTERN" "$TMP/list.json"
    case $? in
      0) in_list=1 ;;
      1) in_list=0 ;;
      *) in_list=error ;;
    esac
  fi
  if [[ "$in_list" == "unknown" ]]; then
    say "  ❌ hide-failed: /api/nodes HTTP $list_code — listing not checked"
    fails=$((fails+1))
  elif [[ "$in_list" == "error" ]]; then
    say "  ❌ hide-failed: could not search /api/nodes response (grep error)"
    fails=$((fails+1))
  elif [[ "$detail_code" == "404" && "$in_list" == "0" ]]; then
    say "  ✅ hide ok: detail=$detail_code in_list=$in_list"
  else
    say "  ❌ hide-failed: detail=$detail_code in_list=$in_list — pubkey still surfaced"
    fails=$((fails+1))
  fi

  # Anything short of a well-formed topology fails: absence is only proven
  # by a response that could have contained the pubkey.
  fetch_topology
  if [[ "$TOPO_CODE" != "200" ]]; then
    say "  ❌ hide-failed: /api/analytics/topology HTTP $TOPO_CODE — topology not checked"
    fails=$((fails+1))
  elif ! jq -r -n "$TOPOLOGY_PUBKEYS_JQ" "$TMP/topo.json" >"$TMP/topo.pubkeys" 2>"$TMP/topo.err"; then
    say "  ❌ hide-failed: /api/analytics/topology is not the expected topology JSON — topology not checked"
    fails=$((fails+1))
  else
    file_has_line "$PK_PATTERN" "$TMP/topo.pubkeys"
    case $? in
      0) say "  ❌ hide-failed: /api/analytics/topology lists the blacklisted pubkey"
         fails=$((fails+1)) ;;
      1) say "  ✅ topology clean ($(grep -c . "$TMP/topo.pubkeys") pubkey fields checked)" ;;
      *) say "  ❌ hide-failed: could not search /api/analytics/topology response (grep error)"
         fails=$((fails+1)) ;;
    esac
  fi

  # ---------------------------------------------------------------------------
  # §10.2 — DB retain
  # ---------------------------------------------------------------------------
  say "=== §10.2 verify packets retained in DB ==="
  if ! read_retain_count; then
    # read_retain_count already printed the classified reason. Counting here and
    # nowhere else: an older version incremented $fails for the "no db path"
    # case and then again for the empty count it left behind.
    fails=$((fails+1))
  elif [[ "$RETAIN_COUNT" =~ ^[0-9]+$ ]] && (( RETAIN_COUNT > 0 )); then
    say "  ✅ DB retains $RETAIN_COUNT packets from $TEST_PUBKEY"
  else
    say "  ❌ retain-failed: count=$RETAIN_COUNT (expected > 0)"
    fails=$((fails+1))
  fi

  say "=== summary: $fails failure(s) before teardown ==="
  # trap handles teardown + exit
  exit "$fails"
}

# Only run main when executed directly (not when sourced by tests).
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main "$@"
fi
