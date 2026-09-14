#!/usr/bin/env bash
# scripts/test-docker-mosquitto-compat.sh
#
# Runtime regression test for the in-container Mosquitto broker of a built
# CoreScope image (Dockerfile / Dockerfile.go runtime stage). It guards two
# Mosquitto 2.1 behaviour changes that arrived with the Alpine 3.24 runtime:
#
#   1. allow_duplicate_messages defaults to true in 2.1. The ingestor can hold
#      overlapping subscriptions (meshcore/+/+/packets + meshcore/#), so every
#      packet was delivered twice: packetsTotal, tx_dupes, observer upserts and
#      observation rows doubled.
#   2. Mosquitto 2.1 drops privileges to PUID/PGID when they are set.
#      entrypoint-go.sh exports /app/data/.env, so a PUID there made the broker
#      run as that uid and fail to write /var/lib/mosquitto/mosquitto.db.
#
# Behavioural, not a config grep: it starts the image with synthetic data and
# --network none, publishes synthetic messages to the in-container broker and
# checks broker deliveries, ingestor counters, the broker's uid/gid and
# persistence across a broker restart. One container runs at a time.
#
# usage: scripts/test-docker-mosquitto-compat.sh <image> [platform]
#   e.g. scripts/test-docker-mosquitto-compat.sh corescope-go:latest linux/arm64
# Containers are created with --rm, so they and their anonymous volumes are
# removed when the test stops them. LOG_DIR=<dir> keeps each container's logs;
# WAIT_S (default 240) bounds every wait (raise it for emulated platforms).
set -uo pipefail

IMAGE=${1:?usage: $0 <image> [platform]}
PLATFORM=${2:-}
WAIT_S=${WAIT_S:-240}
LOG_DIR=${LOG_DIR:-}
RUN_ID="mqttcompat-$$"
OBSERVER=00000000000000000000000000000000000000000000000000000000c0ffee01
TOPIC="meshcore/TST/$OBSERVER/packets"
PAYLOAD='{"raw":"0A00D69FD7A5A7475DB07337749AE61FA53A4788E976","SNR":5.0,"RSSI":-100.0,"origin":"mqtt-compat-test"}'
RETAINED_TOPIC="corescope-compat/retained"
RETAINED_MSG="retained-$RUN_ID"
CHECKS=0
FAILS=0
CONTAINERS=""
TMP=$(mktemp -d)

stop_container() { # name: keep its logs (LOG_DIR), stop it (--rm removes it)
    local c=$1 x rest=""
    if [ -n "$LOG_DIR" ]; then
        mkdir -p "$LOG_DIR"
        docker logs -t "$c" > "$LOG_DIR/$c.log" 2>&1
    fi
    docker stop -t 30 "$c" > /dev/null 2>&1
    for x in $CONTAINERS; do [ "$x" = "$c" ] || rest="$rest $x"; done
    CONTAINERS=$rest
}

cleanup() {
    local c
    for c in $CONTAINERS; do stop_container "$c"; done
    rm -rf "$TMP"
}
trap cleanup EXIT

check() { # description expected actual
    CHECKS=$((CHECKS + 1))
    if [ "$2" = "$3" ]; then
        echo "ok   - $1 ($3)"
    else
        echo "FAIL - $1: expected '$2', got '$3'"
        FAILS=$((FAILS + 1))
    fi
}

wait_for() { # timeout_s command...
    local t=$1 i
    shift
    for ((i = 0; i < t; i++)); do
        "$@" > /dev/null 2>&1 && return 0
        sleep 1
    done
    return 1
}

dx() { docker exec "$@"; }

start_container() { # name [file copied into /app/data ...]
    local name=$1 f
    shift
    local plat=()
    [ -n "$PLATFORM" ] && plat=(--platform "$PLATFORM")
    docker create --rm --name "$name" --network none ${plat[@]+"${plat[@]}"} "$IMAGE" > /dev/null || return 1
    CONTAINERS="$CONTAINERS $name"
    for f in "$@"; do
        docker cp "$f" "$name:/app/data/" > /dev/null || return 1
    done
    docker start "$name" > /dev/null
}

json_int() { grep -oE "\"$1\":[0-9]+" | head -1 | cut -d: -f2; }
server_up() { dx "$1" wget -qO /dev/null http://localhost:3000/api/mqtt/status; }
subscribed_topics() { docker logs "$1" 2>&1 | grep -oE 'MQTT \[[a-z]+\] subscribed to [^ ]+' | sort -u | wc -l | tr -d ' '; }
subscribed_at_least() { [ "$(subscribed_topics "$1")" -ge "$2" ]; }
stats_file_int() { dx "$1" cat /tmp/corescope-ingestor-stats.json 2> /dev/null | json_int "$2"; }
obs_at_least() { [ "$(stats_file_int "$1" obs_inserted)" -ge "$2" ] 2> /dev/null; }
no_subscriber_running() { ! dx "$1" pidof mosquitto_sub; }
ids_of() { dx "$1" awk '/^Uid:/{u=$2} /^Gid:/{g=$2} END{print u ":" g}' "/proc/$2/status"; }
env_has() { dx "$1" sh -c "tr '\\0' '\\n' < /proc/$2/environ | grep -qx '$3'"; }
broker_restarted() { local p; p=$(dx "$1" pidof mosquitto) && [ -n "$p" ] && [ "$p" != "$2" ]; }
retained_present() { [ "$(dx "$1" mosquitto_sub -h localhost -t "$RETAINED_TOPIC" -C 1 -W 3 2> /dev/null)" = "$RETAINED_MSG" ]; }

# Broker identity + persistence: publish a retained message, SIGTERM the broker
# (it saves its database on exit), let supervisord restart it, then require the
# retained message, the database owner and a clean log.
check_broker_persistence() { # container label
    local c=$1 label=$2 want pid newpid
    want=$(dx "$c" sh -c 'echo "$(id -u mosquitto):$(id -g mosquitto)"')
    pid=$(dx "$c" pidof mosquitto)
    check "$label: broker runs as the mosquitto user" "$want" "$(ids_of "$c" "$pid")"
    dx "$c" mosquitto_pub -h localhost -q 1 -r -t "$RETAINED_TOPIC" -m "$RETAINED_MSG"
    dx "$c" kill -TERM "$pid"
    if ! wait_for "$WAIT_S" broker_restarted "$c" "$pid"; then
        check "$label: supervisord restarted the broker" "yes" "no"
        return
    fi
    newpid=$(dx "$c" pidof mosquitto)
    check "$label: restarted broker runs as the mosquitto user" "$want" "$(ids_of "$c" "$newpid")"
    wait_for 60 retained_present "$c"
    check "$label: retained message survives a broker restart" "$RETAINED_MSG" \
        "$(dx "$c" mosquitto_sub -h localhost -t "$RETAINED_TOPIC" -C 1 -W 5 2> /dev/null)"
    check "$label: persistence database owned by the broker user" "$want" \
        "$(dx "$c" stat -c '%u:%g' /var/lib/mosquitto/mosquitto.db 2> /dev/null)"
    check "$label: no persistence write errors logged" "0" \
        "$(docker logs "$c" 2>&1 | grep -cE 'Error saving|Permission denied')"
}

echo "# image: $IMAGE  platform: ${PLATFORM:-default}"

# ---------------------------------------------------------------------------
echo "# case 1: overlapping subscriptions, no PUID/PGID"
C1="corescope-$RUN_ID-overlap"
mkdir -p "$TMP/overlap"
cat > "$TMP/overlap/config.json" << 'EOF'
{"dbPath": "/app/data/meshcore.db",
 "mqttSources": [{"name": "local", "broker": "mqtt://localhost:1883",
                  "topics": ["meshcore/+/+/packets", "meshcore/#"], "connectTimeoutSec": 30}]}
EOF
start_container "$C1" "$TMP/overlap/config.json" || { echo "FAIL - could not start $C1"; exit 1; }
wait_for "$WAIT_S" server_up "$C1" || check "case 1: server ready" "yes" "no"
wait_for "$WAIT_S" subscribed_at_least "$C1" 2 || check "case 1: ingestor subscribed to both topics" "2" "$(subscribed_topics "$C1")"
dx -d "$C1" sh -c "mosquitto_sub -h localhost -v -t 'meshcore/+/+/packets' -t 'meshcore/#' -W 20 > /tmp/compat-overlap.txt 2>&1"
dx -d "$C1" sh -c "mosquitto_sub -h localhost -v -t 'meshcore/#' -W 20 > /tmp/compat-single.txt 2>&1"
sleep 5
dx "$C1" mosquitto_pub -h localhost -q 0 -t "$TOPIC" -m "$PAYLOAD"
wait_for "$WAIT_S" obs_at_least "$C1" 1 || check "case 1: ingestor stored the packet" "yes" "no"
wait_for 60 no_subscriber_running "$C1"
sleep 12 # > /api/stats cache TTL (10s); the ingestor stats file is rewritten every second
check "case 1: deliveries to one client with overlapping subscriptions" "1" "$(dx "$C1" grep -c "^$TOPIC " /tmp/compat-overlap.txt)"
check "case 1: deliveries to a client with a single subscription" "1" "$(dx "$C1" grep -c "^$TOPIC " /tmp/compat-single.txt)"
check "case 1: /api/mqtt/status packetsTotal" "1" "$(dx "$C1" wget -qO- http://localhost:3000/api/mqtt/status | json_int packetsTotal)"
check "case 1: ingestor tx_inserted" "1" "$(stats_file_int "$C1" tx_inserted)"
check "case 1: ingestor tx_dupes" "0" "$(stats_file_int "$C1" tx_dupes)"
check "case 1: ingestor obs_inserted" "1" "$(stats_file_int "$C1" obs_inserted)"
check "case 1: ingestor observer_upserts" "1" "$(stats_file_int "$C1" observer_upserts)"
STATS=$(dx "$C1" wget -qO- http://localhost:3000/api/stats)
check "case 1: /api/stats totalTransmissions" "1" "$(echo "$STATS" | json_int totalTransmissions)"
check "case 1: /api/stats totalObservations" "1" "$(echo "$STATS" | json_int totalObservations)"
check_broker_persistence "$C1" "case 1"
stop_container "$C1"

# ---------------------------------------------------------------------------
echo "# case 2: PUID=1000 PGID=1000 in /app/data/.env (exported by the entrypoint)"
C2="corescope-$RUN_ID-puid"
mkdir -p "$TMP/puid"
printf 'PUID=1000\nPGID=1000\n' > "$TMP/puid/.env"
start_container "$C2" "$TMP/puid/.env" || { echo "FAIL - could not start $C2"; exit 1; }
wait_for "$WAIT_S" server_up "$C2" || check "case 2: server ready" "yes" "no"
wait_for "$WAIT_S" subscribed_at_least "$C2" 1 || check "case 2: ingestor subscribed" "1" "$(subscribed_topics "$C2")"
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
wait_for "$WAIT_S" obs_at_least "$C2" 1
check "case 2: ingestor stored the packet via the broker" "1" "$(stats_file_int "$C2" tx_inserted)"
check_broker_persistence "$C2" "case 2"
stop_container "$C2"

echo "# $CHECKS checks, $FAILS failed"
if [ "$FAILS" -eq 0 ]; then echo "RESULT: PASS"; else echo "RESULT: FAIL"; fi
[ "$FAILS" -eq 0 ]
