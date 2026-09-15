#!/usr/bin/env bash
# scripts/test-docker-mosquitto-compat.sh
#
# Runtime regression test for the in-container Mosquitto broker of a built
# CoreScope image (Dockerfile / Dockerfile.go runtime stage). It guards two
# Mosquitto 2.1 behaviour changes that arrived with the Alpine 3.24 runtime:
#
#   1. allow_duplicate_messages defaults to true in 2.1. The ingestor can hold
#      overlapping subscriptions (meshcore/+/+/packets + meshcore/#), so every
#      packet was delivered twice: packetsTotal, observation rows and observer
#      upserts doubled and tx_dupes went from 0 to 1.
#   2. Mosquitto 2.1 drops privileges to PUID/PGID when they are set.
#      entrypoint-go.sh exports /app/data/.env, so a PUID there made the broker
#      run as that uid and fail to write /var/lib/mosquitto/mosquitto.db.
#
# Behavioural, not a config grep: it starts the image with synthetic data and
# --network none, publishes synthetic messages to the in-container broker and
# checks broker deliveries, ingestor counters, the broker's uid/gid and
# persistence across a broker restart. One container runs at a time.
# Scope: the default supervisord start path only. It does not exercise
# DISABLE_CADDY, does not watch for unexpected restarts of other supervisord
# programs, and a PASS is no evidence that Caddy starts without crashing.
#
# usage: scripts/test-docker-mosquitto-compat.sh <image> [platform]
#   e.g. scripts/test-docker-mosquitto-compat.sh corescope-go:latest linux/arm64
#
# The image must already exist locally. It is never pulled: the reference is
# resolved to its image ID first and every container is created from that ID
# with --pull never. Containers carry the labels com.corescope.test=mosquitto-compat
# and com.corescope.test.run=<RUN_ID>. On normal exit, failures and
# SIGINT/SIGTERM/SIGHUP the test keeps each container's logs, removes exactly
# its own labelled containers and their anonymous volumes, and verifies they
# are gone. A container or volume only counts as gone when a successful docker
# listing no longer contains it; when docker cannot answer, cleanup is reported
# as unresolved and the run fails. If the case 1 container cannot be removed,
# case 2 is never started. Status, RESULT and cleanup lines go to the original
# stdout even when a signal interrupts a redirected docker call, and child
# processes that ignore TERM are killed after a short grace period. SIGKILL
# cannot run any of this, so still run the test under an outer timeout;
# leftovers of a killed run can be listed with
#   docker ps -a --filter label=com.corescope.test.run=<RUN_ID>
#
# Requires bash, the docker CLI and ps supporting -o ppid,lstart,stat.
#
# Environment (wall-clock seconds):
#   TOTAL_S  budget for the whole run, including a 45s cleanup reserve (default 900)
#   WAIT_S   limit for each readiness/progress wait (default 240)
#   EXEC_S   limit for each single docker command (default 30)
#   LOG_DIR  where container logs are kept
#            (default ${TMPDIR:-/tmp}/corescope-mqtt-compat-<RUN_ID>)
#
# Exit status: 0 all 28 checks and all 15 preconditions ran and passed and
# cleanup was verified; 1 a check or precondition failed, the budget ran out, or
# the run stopped early or was incomplete; 2 setup error (bad arguments, no
# docker, image not local, platform mismatch, unsuitable ps); 3 all checks
# passed but cleanup failed or could not be verified; 128+N stopped by signal N.
set -uo pipefail
# Keep the original stdout/stderr: the traps write through them even when a
# signal lands inside a function call whose output is redirected.
exec 3>&1 4>&2

IMAGE=${1:-}
PLATFORM=${2:-}
TOTAL_S=${TOTAL_S:-900}
WAIT_S=${WAIT_S:-240}
EXEC_S=${EXEC_S:-30}
CLEANUP_RESERVE_S=45
EXPECTED_CHECKS=28
EXPECTED_PRECONDITIONS=15
RUN_ID="mqttcompat-$(date +%Y%m%d%H%M%S)-$$-$RANDOM"
LABEL_TEST="com.corescope.test"
LABEL_RUN="com.corescope.test.run"
LOG_DIR=${LOG_DIR:-${TMPDIR:-/tmp}/corescope-mqtt-compat-$RUN_ID}
OBSERVER=00000000000000000000000000000000000000000000000000000000c0ffee01
TOPIC="meshcore/TST/$OBSERVER/packets"
PAYLOAD='{"raw":"0A00D69FD7A5A7475DB07337749AE61FA53A4788E976","SNR":5.0,"RSSI":-100.0,"origin":"mqtt-compat-test"}'
RETAINED_TOPIC="corescope-compat/retained"
RETAINED_MSG="retained-$RUN_ID"

CHECKS=0
FAILS=0
PRECONDITIONS=0
PRECONDITION_FAILS=0
COMPLETED=0
BUDGET_EXHAUSTED=0
CLEANUP_FAILED=0 # an earlier cleanup step failed; a later successful pass never clears it
CLEANUP_DONE=0
RESULT_PRINTED=0
IN_CLEANUP=0
OWNED=""         # IDs of the containers this run created
OWNED_VOLUMES="" # anonymous volumes those containers were created with
LAST_ID=""
BG_PID=""        # background child of this shell, with the ps identity recorded at spawn
BG_IDENT=""
TMP=""

setup_error() {
    echo "RESULT: ERROR - $1"
    RESULT_PRINTED=1
    exit 2
}

case "$TOTAL_S$WAIT_S$EXEC_S" in *[!0-9]*) setup_error "TOTAL_S, WAIT_S and EXEC_S must be whole seconds" ;; esac
[ -n "$IMAGE" ] || setup_error "usage: $0 <image> [platform]"
[ "$TOTAL_S" -gt $((CLEANUP_RESERVE_S + 10)) ] || setup_error "TOTAL_S must be greater than $((CLEANUP_RESERVE_S + 10))"
DEADLINE=$((SECONDS + TOTAL_S - CLEANUP_RESERVE_S))
CLEANUP_DEADLINE=$((SECONDS + TOTAL_S))

# ---------------------------------------------------------------------------
# Wall-clock bounded execution

# A process identity is its parent PID plus start time as reported by ps.
# Signals are only sent while a PID still has the identity recorded when this
# script spawned it, so a recycled PID is never signalled.
proc_identity() { ps -o ppid= -o lstart= -p "$1" 2> /dev/null | tr -s ' '; }
proc_zombie() { case "$(ps -o stat= -p "$1" 2> /dev/null)" in *Z*) return 0 ;; *) return 1 ;; esac; }
proc_alive() { [ -n "$2" ] && [ "$(proc_identity "$1")" = "$2" ] && ! proc_zombie "$1"; }

# stop_own_child <pid> <identity> <grace>: TERM, KILL after <grace> seconds, then
# reap. Never waits unbounded: returns 1 if the process is still there 2s after
# KILL, or if it cannot be verified as ours (no recorded identity).
stop_own_child() {
    local pid=$1 ident=$2 grace=$3 end
    [ -n "$pid" ] || return 0
    if ! proc_alive "$pid" "$ident"; then
        if kill -0 "$pid" 2> /dev/null && ! proc_zombie "$pid"; then
            [ -n "$ident" ] && return 0 # the PID now belongs to another process: not ours
            return 1                    # no recorded identity: do not signal, report it
        fi
        wait "$pid" 2> /dev/null
        return 0
    fi
    kill -TERM "$pid" 2> /dev/null
    end=$((SECONDS + grace))
    while proc_alive "$pid" "$ident"; do
        if [ "$SECONDS" -ge "$end" ]; then
            kill -KILL "$pid" 2> /dev/null
            break
        fi
        sleep 0.2
    done
    end=$((SECONDS + 2))
    while proc_alive "$pid" "$ident"; do
        [ "$SECONDS" -lt "$end" ] || return 1
        sleep 0.2
    done
    wait "$pid" 2> /dev/null
    return 0
}

# run_bounded <limit> <command...>: run an external command with a wall-clock
# limit, capped by what is left of the run's budget. Returns 124 on timeout.
run_bounded() {
    local limit=$1 left end pid ident rc
    shift
    if [ "$IN_CLEANUP" = 1 ]; then left=$((CLEANUP_DEADLINE - SECONDS)); else left=$((DEADLINE - SECONDS)); fi
    [ "$left" -ge "$limit" ] || limit=$left
    [ "$limit" -gt 0 ] || return 124
    "$@" 3>&- 4>&- &
    pid=$!
    ident=$(proc_identity "$pid")
    BG_PID=$pid
    BG_IDENT=$ident
    end=$((SECONDS + limit))
    while kill -0 "$pid" 2> /dev/null; do
        if [ "$SECONDS" -ge "$end" ]; then
            stop_own_child "$pid" "$ident" 2
            BG_PID=""
            BG_IDENT=""
            return 124
        fi
        sleep 0.2
    done
    wait "$pid"
    rc=$?
    BG_PID=""
    BG_IDENT=""
    return "$rc"
}

pause() { # <seconds>: interruptible sleep, capped by the run's budget
    local s=$1 left=$((DEADLINE - SECONDS))
    [ "$left" -ge "$s" ] || s=$left
    [ "$s" -gt 0 ] || return 0
    sleep "$s" 3>&- 4>&- &
    BG_PID=$!
    BG_IDENT=$(proc_identity "$BG_PID")
    wait "$BG_PID"
    BG_PID=""
    BG_IDENT=""
}

wait_until() { # <limit> <predicate...>: poll until the predicate succeeds; 1 on timeout
    local end=$((SECONDS + $1))
    shift
    while :; do
        "$@" > /dev/null 2>&1 && return 0
        if [ "$SECONDS" -ge "$end" ] || [ "$SECONDS" -ge "$DEADLINE" ]; then return 1; fi
        pause 1
    done
}

# ---------------------------------------------------------------------------
# Results

begin_cleanup() { # once: ignore further signals and switch to the cleanup reserve
    [ "$IN_CLEANUP" = 1 ] && return 0
    trap '' INT TERM HUP
    IN_CLEANUP=1
    CLEANUP_DEADLINE=$((SECONDS + CLEANUP_RESERVE_S))
}

cleanup_summary() { # <state of the last cleanup pass>
    local note=""
    [ "$CLEANUP_FAILED" = 0 ] || note=" (an earlier cleanup step failed)"
    echo "# checks: $CHECKS of $EXPECTED_CHECKS run, $FAILS failed; preconditions: $((PRECONDITIONS - PRECONDITION_FAILS)) of $EXPECTED_PRECONDITIONS met; cleanup: $1$note"
}

finish() {
    local met=$((PRECONDITIONS - PRECONDITION_FAILS)) state=verified
    exec 1>&3 2>&4
    begin_cleanup
    cleanup || state="FAILED or unresolved"
    CLEANUP_DONE=1
    cleanup_summary "$state"
    RESULT_PRINTED=1
    if [ "$COMPLETED" != 1 ]; then
        if [ "$FAILS" -ne 0 ] || [ "$PRECONDITION_FAILS" -ne 0 ] || [ "$BUDGET_EXHAUSTED" -ne 0 ] || [ "$CLEANUP_FAILED" -ne 0 ]; then
            echo "RESULT: FAIL (run incomplete)"
        else
            echo "RESULT: INCOMPLETE"
        fi
        exit 1
    fi
    if [ "$FAILS" -ne 0 ] || [ "$PRECONDITION_FAILS" -ne 0 ] || [ "$BUDGET_EXHAUSTED" -ne 0 ]; then
        echo "RESULT: FAIL"
        exit 1
    fi
    if [ "$CHECKS" -ne "$EXPECTED_CHECKS" ] || [ "$met" -ne "$EXPECTED_PRECONDITIONS" ]; then
        echo "RESULT: INCOMPLETE"
        exit 1
    fi
    if [ "$state" != verified ] || [ "$CLEANUP_FAILED" -ne 0 ]; then
        echo "RESULT: FAIL (all checks passed, cleanup failed or unresolved)"
        exit 3
    fi
    echo "RESULT: PASS"
    exit 0
}

budget_exhausted() {
    echo "FAIL - time budget exhausted (TOTAL_S=${TOTAL_S}s incl. ${CLEANUP_RESERVE_S}s cleanup reserve)"
    BUDGET_EXHAUSTED=1
    finish
}

check() { # description expected actual
    [ "$SECONDS" -lt "$DEADLINE" ] || budget_exhausted
    CHECKS=$((CHECKS + 1))
    if [ "$2" = "$3" ]; then
        echo "ok   - $1 ($3)"
    else
        echo "FAIL - $1: expected '$2', got '$3'"
        FAILS=$((FAILS + 1))
    fi
}

precondition_failed() { # description detail
    PRECONDITION_FAILS=$((PRECONDITION_FAILS + 1))
    echo "FAIL - [precondition] $1: $2"
    [ "$SECONDS" -lt "$DEADLINE" ] || budget_exhausted
    finish
}

require() { # description limit predicate...: poll the predicate, stop the run if it never holds
    local desc=$1 limit=$2
    shift 2
    PRECONDITIONS=$((PRECONDITIONS + 1))
    if wait_until "$limit" "$@"; then
        echo "ok   - [precondition] $desc"
    elif [ "$SECONDS" -ge "$DEADLINE" ]; then
        precondition_failed "$desc" "stopped when the time budget ran out"
    else
        precondition_failed "$desc" "not met within ${limit}s"
    fi
}

require_now() { # description command...: one attempt, stop the run if it fails
    local desc=$1
    shift
    PRECONDITIONS=$((PRECONDITIONS + 1))
    if "$@"; then
        echo "ok   - [precondition] $desc"
    else
        precondition_failed "$desc" "failed"
    fi
}

# ---------------------------------------------------------------------------
# Containers owned by this run

volumes_of() { run_bounded "$EXEC_S" docker container inspect -f '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}' "$1"; }

# container_state / volume_state <id|name>: prints present, absent or unknown.
# Absence is only concluded from a successful listing that does not contain the
# object; a failed or timed-out docker command yields unknown.
container_state() {
    local out
    out=$(run_bounded "$EXEC_S" docker ps -a -q --no-trunc) || { echo unknown; return; }
    if printf '%s\n' "$out" | grep -qxF "$1"; then echo present; else echo absent; fi
}

volume_state() {
    local out
    out=$(run_bounded "$EXEC_S" docker volume ls -q) || { echo unknown; return; }
    if printf '%s\n' "$out" | grep -qxF "$1"; then echo present; else echo absent; fi
}

start_container() { # name [file copied into /app/data ...]; sets LAST_ID
    local name=$1 id vols f
    shift
    local plat=()
    [ -n "$PLATFORM" ] && plat=(--platform "$PLATFORM")
    LAST_ID=""
    id=$(run_bounded 60 docker create --pull never --name "$name" \
        --label "$LABEL_TEST=mosquitto-compat" --label "$LABEL_RUN=$RUN_ID" \
        --network none ${plat[@]+"${plat[@]}"} "$IMAGE_ID")
    if [ -z "$id" ]; then
        echo "# could not create $name from $IMAGE_ID"
        return 1
    fi
    OWNED="$OWNED $id"
    LAST_ID=$id
    if ! vols=$(volumes_of "$id"); then
        echo "# could not read the volumes of $name"
        return 1
    fi
    OWNED_VOLUMES="$OWNED_VOLUMES $vols"
    if [ "$(run_bounded "$EXEC_S" docker container inspect -f '{{.Image}}' "$id")" != "$IMAGE_ID" ]; then
        echo "# $name was not created from $IMAGE_ID"
        return 1
    fi
    for f in "$@"; do
        if ! run_bounded "$EXEC_S" docker cp "$f" "$id:/app/data/" > /dev/null; then
            echo "# could not copy $(basename "$f") into $name:/app/data/"
            return 1
        fi
    done
    if ! run_bounded 60 docker start "$id" > /dev/null; then
        echo "# could not start $name"
        return 1
    fi
}

remove_volume() { # anonymous volume recorded from one of this run's containers
    local v=$1 labels users
    case "$(volume_state "$v")" in
        absent) return 0 ;;
        present) ;;
        *) echo "CLEANUP UNRESOLVED - could not determine whether volume $v still exists; left untouched"; return 1 ;;
    esac
    if ! labels=$(run_bounded "$EXEC_S" docker volume inspect -f '{{json .Labels}}' "$v"); then
        echo "CLEANUP UNRESOLVED - could not inspect volume $v; left untouched"
        return 1
    fi
    case "$labels" in
        *'"com.docker.volume.anonymous"'*) ;;
        *) echo "CLEANUP SKIP - volume $v is not marked anonymous; left untouched"; return 1 ;;
    esac
    if ! users=$(run_bounded "$EXEC_S" docker ps -a -q --no-trunc --filter "volume=$v"); then
        echo "CLEANUP UNRESOLVED - could not list the users of volume $v; left untouched"
        return 1
    fi
    if [ -n "$users" ]; then
        echo "CLEANUP SKIP - volume $v is still used by $users; left untouched"
        return 1
    fi
    run_bounded "$EXEC_S" docker volume rm "$v" > /dev/null 2>&1
    case "$(volume_state "$v")" in
        absent) return 0 ;;
        present) echo "CLEANUP FAIL - volume $v still exists"; return 1 ;;
        *) echo "CLEANUP UNRESOLVED - could not confirm that volume $v is gone"; return 1 ;;
    esac
}

remove_container() { # container id: verify the run label, keep logs, remove it and its anonymous volumes
    local id=$1 label name vols vols_known=1 v ok=0
    case "$(container_state "$id")" in
        absent) echo "# container $id is already gone"; return 0 ;;
        present) ;;
        *) echo "CLEANUP UNRESOLVED - could not determine whether container $id still exists; left untouched"; return 1 ;;
    esac
    if ! label=$(run_bounded "$EXEC_S" docker container inspect -f "{{index .Config.Labels \"$LABEL_RUN\"}}" "$id"); then
        echo "CLEANUP UNRESOLVED - could not read the labels of container $id; left untouched"
        return 1
    fi
    if [ "$label" != "$RUN_ID" ]; then
        echo "CLEANUP SKIP - container $id is not labelled $LABEL_RUN=$RUN_ID; left untouched"
        return 1
    fi
    name=$(run_bounded "$EXEC_S" docker container inspect -f '{{.Name}}' "$id" | tr -d '/')
    vols=$(volumes_of "$id") || vols_known=0
    if mkdir -p "$LOG_DIR" 2> /dev/null; then
        run_bounded "$EXEC_S" docker logs -t "$id" > "$LOG_DIR/${name:-$id}.log" 2>&1 ||
            echo "# could not save the logs of ${name:-$id}"
    fi
    run_bounded 20 docker stop -t 10 "$id" > /dev/null 2>&1
    run_bounded "$EXEC_S" docker rm -f -v "$id" > /dev/null 2>&1
    case "$(container_state "$id")" in
        absent) ;;
        present) echo "CLEANUP FAIL - container ${name:-$id} still exists"; return 1 ;;
        *) echo "CLEANUP UNRESOLVED - could not confirm that container ${name:-$id} is gone"; return 1 ;;
    esac
    if [ "$vols_known" = 0 ]; then
        echo "CLEANUP UNRESOLVED - removed container ${name:-$id}, but could not list its volumes to verify them"
        return 1
    fi
    for v in $vols; do
        remove_volume "$v" || ok=1
    done
    [ "$ok" = 0 ] || return 1
    echo "# removed container ${name:-$id} and its anonymous volumes:" $vols
}

forget_container() { # container id
    local x rest=""
    for x in $OWNED; do [ "$x" = "$1" ] || rest="$rest $x"; done
    OWNED=$rest
}

retire_container() { # container id, label: it must be verifiably gone before anything else starts
    if remove_container "$1"; then
        forget_container "$1"
    else
        CLEANUP_FAILED=1
        echo "FAIL - [sequence] $2 container could not be stopped and removed safely; no further containers are started"
        finish
    fi
}

cleanup() { # one cleanup pass; status 1 if anything failed or stayed unresolved in this pass
    local ok=0 id v listed
    if ! listed=$(run_bounded "$EXEC_S" docker ps -a -q --no-trunc --filter "label=$LABEL_RUN=$RUN_ID"); then
        echo "CLEANUP UNRESOLVED - could not list containers labelled $LABEL_RUN=$RUN_ID"
        listed=""
        ok=1
    fi
    for id in $(printf '%s\n' $OWNED $listed | sort -u); do
        if remove_container "$id"; then forget_container "$id"; else ok=1; fi
    done
    for v in $(printf '%s\n' $OWNED_VOLUMES | sort -u); do
        remove_volume "$v" || ok=1
    done
    return "$ok"
}

on_signal() { # number name
    exec 1>&3 2>&4
    echo "# received SIG$2; stopping and cleaning up"
    exit $((128 + $1))
}

on_exit() {
    local rc=$? state=verified
    exec 1>&3 2>&4
    begin_cleanup
    if [ -n "$BG_PID" ]; then
        if ! stop_own_child "$BG_PID" "$BG_IDENT" 3; then
            echo "CLEANUP UNRESOLVED - own child process $BG_PID could not be stopped and verified"
            CLEANUP_FAILED=1
        fi
        BG_PID=""
        BG_IDENT=""
    fi
    if [ "$CLEANUP_DONE" != 1 ]; then
        cleanup || state="FAILED or unresolved"
        CLEANUP_DONE=1
        if [ "$RESULT_PRINTED" != 1 ]; then
            cleanup_summary "$state"
            echo "RESULT: INCOMPLETE"
            [ "$rc" -ne 0 ] || rc=1
        elif [ "$state" != verified ]; then
            echo "CLEANUP FAIL - leftovers may carry the label $LABEL_RUN=$RUN_ID"
            [ "$rc" -ne 0 ] || rc=3
        fi
    fi
    if [ "$CLEANUP_FAILED" != 0 ] && [ "$rc" -eq 0 ]; then rc=3; fi
    [ -z "$TMP" ] || rm -rf "$TMP"
    exit "$rc"
}

# ---------------------------------------------------------------------------
# Probes inside a running container

dx() { run_bounded "$EXEC_S" docker exec "$@"; }
dlogs() { run_bounded "$EXEC_S" docker logs "$@"; }
json_int() { grep -oE "\"$1\":[0-9]+" | head -1 | cut -d: -f2; }
server_up() { dx "$1" wget -T 5 -qO /dev/null http://localhost:3000/api/mqtt/status; }
subscribed_topics() { dlogs "$1" 2>&1 | grep -oE 'MQTT \[[a-z]+\] subscribed to [^ ]+' | sort -u | wc -l | tr -d ' '; }
subscribed_at_least() { [ "$(subscribed_topics "$1")" -ge "$2" ]; }
stats_file_int() { dx "$1" cat /tmp/corescope-ingestor-stats.json 2> /dev/null | json_int "$2"; }
obs_at_least() { [ "$(stats_file_int "$1" obs_inserted)" -ge "$2" ] 2> /dev/null; }
no_subscriber_running() { dx "$1" pidof mosquitto_sub > /dev/null; [ $? -eq 1 ]; }
ids_of() { dx "$1" awk '/^Uid:/{u=$2} /^Gid:/{g=$2} END{print u ":" g}' "/proc/$2/status"; }
env_has() { dx "$1" sh -c "tr '\\0' '\\n' < /proc/$2/environ | grep -qx '$3'"; }
broker_restarted() { local p; p=$(dx "$1" pidof mosquitto) && [ -n "$p" ] && [ "$p" != "$2" ]; }
retained_present() { [ "$(dx "$1" mosquitto_sub -h localhost -t "$RETAINED_TOPIC" -C 1 -W 3 2> /dev/null)" = "$RETAINED_MSG" ]; }
is_uid_gid() { case "$1" in [0-9]*:[0-9]*) return 0 ;; *) return 1 ;; esac; }

# Broker identity + persistence: publish a retained message, SIGTERM the broker
# (it saves its database on exit), let supervisord restart it, then require the
# retained message, the database owner and a clean log.
check_broker_persistence() { # container-id label
    local c=$1 label=$2 want pid newpid got logs errors
    want=$(dx "$c" sh -c 'echo "$(id -u mosquitto):$(id -g mosquitto)"')
    require_now "$label: image has a mosquitto user (uid:gid '$want')" is_uid_gid "$want"
    pid=$(dx "$c" pidof mosquitto)
    require_now "$label: broker process found" test -n "$pid"
    check "$label: broker runs as the mosquitto user" "$want" "$(ids_of "$c" "$pid")"
    dx "$c" mosquitto_pub -h localhost -q 1 -r -t "$RETAINED_TOPIC" -m "$RETAINED_MSG"
    dx "$c" kill -TERM "$pid"
    require "$label: supervisord restarted the broker" "$WAIT_S" broker_restarted "$c" "$pid"
    newpid=$(dx "$c" pidof mosquitto)
    check "$label: restarted broker runs as the mosquitto user" "$want" "$(ids_of "$c" "$newpid")"
    if wait_until 60 retained_present "$c"; then
        got=$RETAINED_MSG
    else
        got=$(dx "$c" mosquitto_sub -h localhost -t "$RETAINED_TOPIC" -C 1 -W 5 2> /dev/null)
    fi
    check "$label: retained message survives a broker restart" "$RETAINED_MSG" "$got"
    check "$label: persistence database owned by the broker user" "$want" \
        "$(dx "$c" stat -c '%u:%g' /var/lib/mosquitto/mosquitto.db 2> /dev/null)"
    if logs=$(dlogs "$c" 2>&1); then
        errors=$(printf '%s\n' "$logs" | grep -cE 'Error saving|Permission denied')
    else
        errors="unknown (docker logs failed)"
    fi
    check "$label: no persistence write errors logged" "0" "$errors"
}

# ---------------------------------------------------------------------------
# Setup: local image only, resolved once

command -v docker > /dev/null 2>&1 || setup_error "docker CLI not found"
ps -o ppid= -o lstart= -o stat= -p $$ > /dev/null 2>&1 || setup_error "ps with -o ppid,lstart,stat is required to bound child processes"
TMP=$(mktemp -d) || setup_error "mktemp failed"
trap on_exit EXIT
trap 'on_signal 1 HUP' HUP
trap 'on_signal 2 INT' INT
trap 'on_signal 15 TERM' TERM

run_bounded "$EXEC_S" docker version -f '{{.Server.Version}}' > /dev/null 2>&1 || setup_error "docker daemon not reachable"
IMAGE_ID=$(run_bounded "$EXEC_S" docker image inspect -f '{{.Id}}' "$IMAGE" 2> /dev/null)
case "$IMAGE_ID" in
    sha256:*) ;;
    *) setup_error "image '$IMAGE' is not available locally (this test never pulls images)" ;;
esac
IMAGE_PLATFORM=$(run_bounded "$EXEC_S" docker image inspect -f '{{.Os}}/{{.Architecture}}' "$IMAGE_ID")
if [ -n "$PLATFORM" ] && [ "$(printf '%s' "$PLATFORM" | cut -d/ -f1-2)" != "$IMAGE_PLATFORM" ]; then
    setup_error "image $IMAGE_ID is $IMAGE_PLATFORM, not $PLATFORM"
fi
echo "# run: $RUN_ID"
echo "# image: $IMAGE"
echo "# image id: $IMAGE_ID ($IMAGE_PLATFORM)"
echo "# limits: TOTAL_S=${TOTAL_S}s (incl. ${CLEANUP_RESERVE_S}s cleanup reserve), WAIT_S=${WAIT_S}s, EXEC_S=${EXEC_S}s"
echo "# logs: $LOG_DIR"

# ---------------------------------------------------------------------------
echo "# case 1: overlapping subscriptions, no PUID/PGID"
mkdir -p "$TMP/overlap"
cat > "$TMP/overlap/config.json" << 'EOF'
{"dbPath": "/app/data/meshcore.db",
 "mqttSources": [{"name": "local", "broker": "mqtt://localhost:1883",
                  "topics": ["meshcore/+/+/packets", "meshcore/#"], "connectTimeoutSec": 30}]}
EOF
require_now "case 1: container created from $IMAGE_ID and started" start_container "corescope-$RUN_ID-overlap" "$TMP/overlap/config.json"
C1=$LAST_ID
require "case 1: server ready" "$WAIT_S" server_up "$C1"
require "case 1: ingestor subscribed to both topics" "$WAIT_S" subscribed_at_least "$C1" 2
echo "# mosquitto: $(dx "$C1" sh -c 'mosquitto -h 2>&1 | head -1')"
dx -d "$C1" sh -c "mosquitto_sub -h localhost -v -t 'meshcore/+/+/packets' -t 'meshcore/#' -W 20 > /tmp/compat-overlap.txt 2>&1"
dx -d "$C1" sh -c "mosquitto_sub -h localhost -v -t 'meshcore/#' -W 20 > /tmp/compat-single.txt 2>&1"
pause 5 # let both test subscribers connect before publishing
dx "$C1" mosquitto_pub -h localhost -q 0 -t "$TOPIC" -m "$PAYLOAD"
require "case 1: ingestor stored the packet" "$WAIT_S" obs_at_least "$C1" 1
require "case 1: test subscribers finished" 60 no_subscriber_running "$C1"
pause 12 # > /api/stats cache TTL (10s); the ingestor stats file is rewritten every second
check "case 1: deliveries to one client with overlapping subscriptions" "1" "$(dx "$C1" grep -c "^$TOPIC " /tmp/compat-overlap.txt)"
check "case 1: deliveries to a client with a single subscription" "1" "$(dx "$C1" grep -c "^$TOPIC " /tmp/compat-single.txt)"
check "case 1: /api/mqtt/status packetsTotal" "1" "$(dx "$C1" wget -T 5 -qO- http://localhost:3000/api/mqtt/status | json_int packetsTotal)"
check "case 1: ingestor tx_inserted" "1" "$(stats_file_int "$C1" tx_inserted)"
check "case 1: ingestor tx_dupes" "0" "$(stats_file_int "$C1" tx_dupes)"
check "case 1: ingestor obs_inserted" "1" "$(stats_file_int "$C1" obs_inserted)"
check "case 1: ingestor observer_upserts" "1" "$(stats_file_int "$C1" observer_upserts)"
STATS=$(dx "$C1" wget -T 5 -qO- http://localhost:3000/api/stats)
check "case 1: /api/stats totalTransmissions" "1" "$(echo "$STATS" | json_int totalTransmissions)"
check "case 1: /api/stats totalObservations" "1" "$(echo "$STATS" | json_int totalObservations)"
check_broker_persistence "$C1" "case 1"
retire_container "$C1" "case 1"

# ---------------------------------------------------------------------------
echo "# case 2: PUID=1000 PGID=1000 in /app/data/.env (exported by the entrypoint)"
mkdir -p "$TMP/puid"
printf 'PUID=1000\nPGID=1000\n' > "$TMP/puid/.env"
require_now "case 2: container created from $IMAGE_ID and started" start_container "corescope-$RUN_ID-puid" "$TMP/puid/.env"
C2=$LAST_ID
require "case 2: server ready" "$WAIT_S" server_up "$C2"
require "case 2: ingestor subscribed" "$WAIT_S" subscribed_at_least "$C2" 1
check "case 2: supervisord (pid 1) keeps PUID/PGID" "yes" \
    "$(env_has "$C2" 1 PUID=1000 && env_has "$C2" 1 PGID=1000 && echo yes || echo no)"
check "case 2: supervisord still runs as root" "0:0" "$(ids_of "$C2" 1)"
for p in corescope-ingestor corescope-server caddy; do
    pid=$(dx "$C2" pidof "$p")
    check "case 2: $p keeps PUID/PGID" "yes" \
        "$(env_has "$C2" "$pid" PUID=1000 && env_has "$C2" "$pid" PGID=1000 && echo yes || echo no)"
    check "case 2: $p still runs as root" "0:0" "$(ids_of "$C2" "$pid")"
done
dx "$C2" mosquitto_pub -h localhost -q 0 -t "$TOPIC" -m "$PAYLOAD"
require "case 2: ingestor stored the packet" "$WAIT_S" obs_at_least "$C2" 1
check "case 2: ingestor stored the packet via the broker" "1" "$(stats_file_int "$C2" tx_inserted)"
check_broker_persistence "$C2" "case 2"
COMPLETED=1
retire_container "$C2" "case 2"
finish
