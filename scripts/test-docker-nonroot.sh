#!/usr/bin/env bash
# scripts/test-docker-nonroot.sh — runtime test of the opt-in non-root mode
# (#339) against a locally built image (Dockerfile / Dockerfile.go).
#
# usage: scripts/test-docker-nonroot.sh <image> [baseline-image]
#
#   <image>           the image under test; never pulled (--pull never)
#   [baseline-image]  optional image built from the commit before the change;
#                     the "off" start of both images is then compared
#                     snapshot by snapshot and must be identical
#
# Local Docker only: every container runs with --network none on labelled
# named volumes that the test creates and removes. It never touches an
# existing container, volume or deployment.
#
# Cases
#   0  off: unset/empty/0/false x the four DISABLE_* combinations start
#      supervisord exactly as before (stubbed supervisord takes a snapshot);
#      on: the generated config is the selected one plus user= lines
#   1  fresh volume: Go services as UID:GID, Mosquitto as mosquitto, Caddy
#      and supervisord as root; the ingestor stores a packet and the server
#      reads it; supervisor restarts of both programs; docker restart
#   2  existing root-owned DB with WAL/SHM (copied fixture, hard-killed root
#      run): only the listed paths change owner; WAL content survives; no
#      symlink or hard link is followed; a root-written request is set aside
#   3  read-only config mount + PUID/PGID: services read it; geo-filter save
#      fails with 409 and a clear message; the same for a config.json that
#      sits writable in the data volume; an unreadable config fails fast
#   4  toggle off -> on -> off on one volume, in-place toggle via
#      /app/data/.env + docker restart, rollback with owners/restore-owners
#   5  invalid settings fail fast and change nothing
#   6  as the services' user inside the running container: the files root
#      reads cannot be changed, replaced or removed; one created later stops
#      the next start before root uses it; a root admin command is refused
#
# Environment: WAIT_S (default 120) per wait, LOG_DIR for container logs.
# Exit: 0 all checks passed, 1 a check failed, 2 setup error.
# Helpers run indirectly (wait_until, traps); in-container scripts are
# single-quoted on purpose.
# shellcheck disable=SC2317,SC2016
set -uo pipefail

IMAGE=${1:-}
BASELINE=${2:-}
WAIT_S=${WAIT_S:-120}
[ -n "$IMAGE" ] || { echo "usage: $0 <image> [baseline-image]" >&2; exit 2; }
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN_ID="nonroot-$(date +%Y%m%d%H%M%S)-$$"
LABEL="com.corescope.test.run=$RUN_ID"
TMP=$(mktemp -d)
LOG_DIR=${LOG_DIR:-$TMP/logs}
mkdir -p "$LOG_DIR"
U=10001
G=10001
ON=(-e CORESCOPE_RUN_AS_NONROOT=1 -e "CORESCOPE_UID=$U" -e "CORESCOPE_GID=$G")
API_KEY="nonroot-test-only-key-339-0123456789abcdef"
OBSERVER=00000000000000000000000000000000000000000000000000000000c0ffee39
CHECKS=0
FAILS=0

for img in "$IMAGE" $BASELINE; do
    docker image inspect "$img" > /dev/null 2>&1 || { echo "image $img is not available locally" >&2; exit 2; }
done

cleanup() {
    local c v
    for c in $(docker ps -aq --filter "label=$LABEL"); do
        docker logs "$c" > "$LOG_DIR/$c.log" 2>&1
        docker rm -fv "$c" > /dev/null 2>&1
    done
    for v in $(docker volume ls -q --filter "label=$LABEL"); do
        docker volume rm "$v" > /dev/null 2>&1 || echo "cleanup: could not remove volume $v" >&2
    done
    echo "# logs kept in $LOG_DIR"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

check() {
    CHECKS=$((CHECKS + 1))
    if [ "$2" = "$3" ]; then
        echo "ok $CHECKS - $1"
    else
        FAILS=$((FAILS + 1))
        echo "not ok $CHECKS - $1"
        echo "#   expected: $2"
        echo "#   actual:   $3"
    fi
}
check_cmd() { # LABEL CMD... — passes when CMD succeeds
    local label=$1
    shift
    if "$@"; then check "$label" yes yes; else check "$label" yes no; fi
}

wait_until() { # SECONDS CMD...
    local end=$((SECONDS + $1))
    shift
    until "$@" 2> /dev/null; do
        [ "$SECONDS" -lt "$end" ] || return 1
        sleep 2
    done
}

vol() { docker volume create --label "$LABEL" "cs-$RUN_ID-$1" > /dev/null && echo "cs-$RUN_ID-$1"; }
# as_root VOL SCRIPT [docker run args...]: run SCRIPT as root on the volume.
as_root() {
    local v=$1 s=$2
    shift 2
    docker run --rm --label "$LABEL" --pull never --network none -v "$v:/app/data" "$@" \
        --entrypoint sh "$IMAGE" -c "$s"
}
start() { # NAME VOL [docker run args...] — prints the container ID
    local n=$1 v=$2
    shift 2
    docker run -d --name "cs-$RUN_ID-$n" --label "$LABEL" --pull never --network none \
        -v "$v:/app/data" "$@" "$IMAGE"
}
dx() { timeout 60 docker exec "$@"; }
json_int() { grep -oE "\"$1\":[0-9]+" | head -1 | cut -d: -f2; }
server_up() { dx "$1" wget -T 5 -qO /dev/null http://localhost:3000/api/stats; }
subscribed() { docker logs "$1" 2>&1 | grep -q 'subscribed to'; }
api_tx() { dx "$1" wget -T 5 -qO- http://localhost:3000/api/stats | json_int totalTransmissions; }
ing_tx() { dx "$1" cat /tmp/corescope-ingestor-stats.json 2> /dev/null | json_int tx_inserted; }
api_tx_is() { [ "$(api_tx "$1")" = "$2" ]; }
ing_tx_is() { [ "$(ing_tx "$1")" = "$2" ]; }
ids_of() { # CONTAINER PROGRAM -> uid:gid of its process
    dx "$1" sh -c "p=\$(pidof $2) && awk '/^Uid:/{u=\$2} /^Gid:/{g=\$2} END{print u \":\" g}' /proc/\$p/status"
}
pid_of() { dx "$1" pidof "$2"; }
pid_changed() { local p; p=$(pid_of "$1" "$2") && [ -n "$p" ] && [ "$p" != "$3" ]; }
owner() { as_root "$1" "stat -c %u:%g /app/data/$2"; }
tree() { as_root "$1" 'cd /app/data && find . | sort | while read -r f; do stat -c "%n %u:%g %a %F" "$f"; done'; }
ps_dump() {
    echo "# ps ($1):"
    dx "$2" ps -o pid,user,group,args | sed 's/^/#   /'
    for p in supervisord corescope-ingestor corescope-server mosquitto caddy; do
        dx "$2" pidof "$p" > /dev/null 2>&1 && echo "#   $p uid:gid $(ids_of "$2" "$p")"
    done
}
publish() { # CONTAINER N: one distinct synthetic packet
    local raw
    raw=$(printf '0A00D69FD7A5A7475DB07337749AE61FA53A4788E9%02X' "$((0x70 + $2))")
    dx "$1" mosquitto_pub -h localhost -q 1 -t "meshcore/TST/$OBSERVER/packets" \
        -m "{\"raw\":\"$raw\",\"SNR\":5.0,\"RSSI\":-100.0,\"origin\":\"nonroot-test\"}"
}
procs_as() { # LABEL CONTAINER GO_IDS — expectations for every program
    check "$1: supervisord" "0:0" "$(ids_of "$2" supervisord)"
    check "$1: corescope-ingestor" "$3" "$(ids_of "$2" corescope-ingestor)"
    check "$1: corescope-server" "$3" "$(ids_of "$2" corescope-server)"
    dx "$2" pidof mosquitto > /dev/null && check "$1: mosquitto" "101:102" "$(ids_of "$2" mosquitto)"
    dx "$2" pidof caddy > /dev/null && check "$1: caddy" "0:0" "$(ids_of "$2" caddy)"
}
ready() { # CONTAINER — server answers and the ingestor subscribed
    wait_until "$WAIT_S" server_up "$1" && wait_until "$WAIT_S" subscribed "$1"
}
geo_put() { # CONTAINER -> "STATUS BODY"
    local body='{"polygon":[[51.0,4.0],[51.0,5.0],[50.5,5.0]],"bufferKm":1}'
    dx "$1" sh -c "printf 'PUT /api/config/geo-filter HTTP/1.0\r\nHost: localhost\r\nX-API-Key: $API_KEY\r\nContent-Type: application/json\r\nContent-Length: ${#body}\r\n\r\n%s' '$body' | nc -w 10 localhost 3000" |
        awk 'NR==1{s=$2} /^\r?$/{b=1; next} b{print s " " $0}' | tr -d '\r'
}

CONFIG="{\"apiKey\":\"$API_KEY\",\"mqttSources\":[{\"name\":\"local\",\"broker\":\"mqtt://localhost:1883\",\"topics\":[\"meshcore/#\"],\"connectTimeoutSec\":30}]}"

echo "# run: $RUN_ID"
echo "# image: $IMAGE ($(docker image inspect -f '{{.Id}}' "$IMAGE"))"
[ -z "$BASELINE" ] || echo "# baseline: $BASELINE ($(docker image inspect -f '{{.Id}}' "$BASELINE"))"

# ---------------------------------------------------------------------------
echo "# case 0: supervisord start path, stubbed"
cat > "$TMP/stub" << 'EOF'
#!/bin/sh
echo "argv: $*"
echo "ids: $(id -u):$(id -g)"
env | LC_ALL=C sort | grep -vE '^(HOSTNAME|PWD|SHLVL|_)=' | sed 's/^/env: /'
echo "passwd: $(md5sum < /etc/passwd)"
echo "group: $(md5sum < /etc/group)"
find /run /tmp /usr/local/lib | LC_ALL=C sort | sed 's/^/path: /'
cd /app && find . -path ./public -prune -o -print | LC_ALL=C sort | while read -r f; do stat -c 'stat: %n %u:%g %a %F %N' "$f"; done
[ "$1" = -c ] && echo "conf: $(md5sum < "$2")"
EOF
chmod 755 "$TMP/stub"
V0=$(vol off)
as_root "$V0" "printf '%s' '$CONFIG' > /app/data/config.json && echo '{}' > /app/data/theme.json &&
    echo 'CORESCOPE_UID=1000' > /app/data/.env && : > /app/data/meshcore.db && : > /app/data/meshcore.db-wal &&
    mkdir /app/data/prune-requests && chmod 640 /app/data/config.json"
V0ON=$(vol on-stub)
as_root "$V0ON" "printf '%s' '$CONFIG' > /app/data/config.json"
snapshot() { # IMAGE VOLUME [docker run args...]
    local img=$1 v=$2
    shift 2
    docker run --rm --label "$LABEL" --pull never --network none -v "$v:/app/data" \
        -v "$TMP/stub:/usr/bin/supervisord:ro" "$@" "$img"
}
PRISTINE_PASSWD=$(docker run --rm --pull never --network none --entrypoint sh "$IMAGE" -c 'md5sum < /etc/passwd')
DATA_BEFORE=$(tree "$V0")
for combo in "none" "DISABLE_MOSQUITTO" "DISABLE_CADDY" "DISABLE_MOSQUITTO DISABLE_CADDY"; do
    dis=()
    for d in $combo; do [ "$d" = none ] || dis+=(-e "$d=true"); done
    case "$combo" in
        none) conf=supervisord.conf ;;
        DISABLE_MOSQUITTO) conf=supervisord-no-mosquitto.conf ;;
        DISABLE_CADDY) conf=supervisord-no-caddy.conf ;;
        *) conf=supervisord-no-mosquitto-no-caddy.conf ;;
    esac
    for setting in unset empty 0 false; do
        env=()
        case "$setting" in unset) ;; empty) env=(-e CORESCOPE_RUN_AS_NONROOT=) ;; *) env=(-e "CORESCOPE_RUN_AS_NONROOT=$setting") ;; esac
        tag="off ($setting, $combo)"
        snap=$(snapshot "$IMAGE" "$V0" "${dis[@]}" "${env[@]}")
        check "$tag: supervisord config" "argv: -c /etc/supervisor/conf.d/$conf" "$(echo "$snap" | grep '^argv:')"
        check "$tag: entrypoint runs as root" "ids: 0:0" "$(echo "$snap" | grep '^ids:')"
        check "$tag: no account created" "passwd: $PRISTINE_PASSWD" "$(echo "$snap" | grep '^passwd:')"
        check "$tag: no generated config" "" "$(echo "$snap" | grep '^path: /run/corescope')"
        if [ -n "$BASELINE" ]; then
            base_snap=$(snapshot "$BASELINE" "$V0" "${dis[@]}" "${env[@]}")
            # The helper file is the only new path in the image.
            diff_out=$(diff <(echo "$base_snap") <(echo "$snap" | grep -v '^path: /usr/local/lib/corescope'))
            check "$tag: identical to baseline image" "" "$diff_out"
        fi
    done
    snap=$(snapshot "$IMAGE" "$V0ON" "${dis[@]}" "${ON[@]}")
    want=$(docker run --rm --pull never --network none --entrypoint /usr/local/lib/corescope/nonroot.sh "$IMAGE" \
        render-conf "/etc/supervisor/conf.d/$conf" corescope | md5sum)
    check "on ($combo): generated config used" "argv: -c /run/corescope/supervisord.conf" "$(echo "$snap" | grep '^argv:')"
    check "on ($combo): generated from $conf" "conf: $want" "$(echo "$snap" | grep '^conf:')"
done
check "off: data volume untouched by all off starts" "$DATA_BEFORE" "$(tree "$V0")"

# ---------------------------------------------------------------------------
echo "# case 1: fresh volume, non-root"
V1=$(vol fresh)
C1=$(start fresh "$V1" "${ON[@]}")
check_cmd "case 1: ready" ready "$C1"
procs_as "case 1" "$C1" "$U:$G"
ps_dump "case 1, fresh volume" "$C1"
check "case 1: data dir keeps its owner, gets the services' group" "0:$G" "$(owner "$V1" .)"
check "case 1: data dir group-writable and sticky" "1775" "$(as_root "$V1" 'stat -c %a /app/data')"
check "case 1: meshcore.db owner" "$U:$G" "$(owner "$V1" meshcore.db)"
publish "$C1" 1
check_cmd "case 1: ingestor stored packet 1" wait_until "$WAIT_S" ing_tx_is "$C1" 1
check_cmd "case 1: server reads packet 1" wait_until "$WAIT_S" api_tx_is "$C1" 1
ING=$(pid_of "$C1" corescope-ingestor)
SRV=$(pid_of "$C1" corescope-server)
dx "$C1" kill -TERM "$ING" "$SRV"
check_cmd "case 1: supervisor restarted the ingestor" wait_until "$WAIT_S" pid_changed "$C1" corescope-ingestor "$ING"
check_cmd "case 1: supervisor restarted the server" wait_until "$WAIT_S" pid_changed "$C1" corescope-server "$SRV"
check_cmd "case 1: server back after program restart" wait_until "$WAIT_S" server_up "$C1"
procs_as "case 1 after program restart" "$C1" "$U:$G"
publish "$C1" 2
check_cmd "case 1: ingestor stored packet 2 after restart" wait_until "$WAIT_S" ing_tx_is "$C1" 1
check_cmd "case 1: server reads 2 packets" wait_until "$WAIT_S" api_tx_is "$C1" 2
timeout 120 docker restart "$C1" > /dev/null
check_cmd "case 1: ready after docker restart" ready "$C1"
procs_as "case 1 after docker restart" "$C1" "$U:$G"
ps_dump "case 1, after docker restart" "$C1"
check_cmd "case 1: second start changes no ownership" sh -c \
    "docker logs '$C1' 2>&1 | grep -E '^\[nonroot\] ownership:' | tail -1 | grep -qE ' 0 of [1-9][0-9]* path'"
publish "$C1" 3
check_cmd "case 1: server reads 3 packets after docker restart" wait_until "$WAIT_S" api_tx_is "$C1" 3
docker rm -fv "$C1" > /dev/null

# ---------------------------------------------------------------------------
echo "# case 2: existing root-owned DB with WAL/SHM"
V2=$(vol rootdb)
docker run --rm --label "$LABEL" --pull never --network none -v "$V2:/app/data" \
    -v "$ROOT/test-fixtures/e2e-fixture.db:/fixture.db:ro" --entrypoint sh "$IMAGE" -c "
    cp /fixture.db /app/data/meshcore.db && printf '%s' '$CONFIG' > /app/data/config.json &&
    echo '{}' > /app/data/theme.json && mkdir -p /app/data/notes && echo keep > /app/data/notes/keep.txt &&
    echo old > /app/data/backup-old.db && echo victim > /app/data/victim.txt && echo victim2 > /app/data/victim2.txt &&
    ln -s /app/data/victim.txt /app/data/client-rx-delete.salt &&
    ln /app/data/victim2.txt /app/data/client-rx-delete-audit.jsonl &&
    mkdir /app/data/prune-requests && echo '{}' > /app/data/prune-requests/request-x.json"
C2=$(start rootdb-root "$V2")
check_cmd "case 2: root run ready" ready "$C2"
BASE_TX=$(api_tx "$C2")
publish "$C2" 4
check_cmd "case 2: root run stored packet 4" wait_until "$WAIT_S" ing_tx_is "$C2" 1
check_cmd "case 2: root run reads it" wait_until "$WAIT_S" api_tx_is "$C2" "$((BASE_TX + 1))"
docker kill -s KILL "$C2" > /dev/null
for f in meshcore.db meshcore.db-wal meshcore.db-shm; do
    check "case 2: precondition $f root-owned after hard kill" "0:0" "$(owner "$V2" "$f")"
done
check "case 2: precondition WAL not empty" "yes" "$(as_root "$V2" '[ -s /app/data/meshcore.db-wal ] && echo yes')"
docker rm -fv "$C2" > /dev/null
UNRELATED_BEFORE=$(tree "$V2" | grep -E '^\./(config\.json|theme\.json|notes|notes/keep\.txt|backup-old\.db|victim\.txt|victim2\.txt|client-rx-delete\.salt|client-rx-delete-audit\.jsonl) ')
C2=$(start rootdb-nonroot "$V2" "${ON[@]}")
check_cmd "case 2: non-root run ready" ready "$C2"
procs_as "case 2" "$C2" "$U:$G"
ps_dump "case 2, migrated root-owned DB" "$C2"
for f in meshcore.db meshcore.db-wal meshcore.db-shm ping_scores_history.db prune-requests; do
    check "case 2: $f migrated" "$U:$G" "$(owner "$V2" "$f")"
done
check "case 2: data dir keeps its owner" "0:$G" "$(owner "$V2" .)"
check "case 2: root-written request no longer pending" "absent" \
    "$(as_root "$V2" '[ -e /app/data/prune-requests/request-x.json ] && echo present || echo absent')"
check "case 2: root-written request set aside, still root's" "0:0" "$(owner "$V2" prune-requests/request-x.json.held)"
check "case 2: unrelated files, symlink and hard link untouched" "$UNRELATED_BEFORE" \
    "$(tree "$V2" | grep -E '^\./(config\.json|theme\.json|notes|notes/keep\.txt|backup-old\.db|victim\.txt|victim2\.txt|client-rx-delete\.salt|client-rx-delete-audit\.jsonl) ')"
check_cmd "case 2: symlink and hard link reported" sh -c "docker logs '$C2' 2>&1 | grep -q 'client-rx-delete.salt is a symlink' &&
    docker logs '$C2' 2>&1 | grep -q 'client-rx-delete-audit.jsonl is hard-linked'"
check_cmd "case 2: WAL content survives (server reads packet 4)" wait_until "$WAIT_S" api_tx_is "$C2" "$((BASE_TX + 1))"
publish "$C2" 5
check_cmd "case 2: non-root ingestor writes the migrated DB" wait_until "$WAIT_S" ing_tx_is "$C2" 1
check_cmd "case 2: server reads it" wait_until "$WAIT_S" api_tx_is "$C2" "$((BASE_TX + 2))"
docker rm -fv "$C2" > /dev/null

# ---------------------------------------------------------------------------
echo "# case 3: read-only config mount, PUID/PGID, geo-filter save"
printf '%s' "$CONFIG" > "$TMP/config.json"
chmod 644 "$TMP/config.json"
CFG_SUM=$(md5sum < "$TMP/config.json")
V3=$(vol roconfig)
C3=$(start roconfig "$V3" "${ON[@]}" -e PUID=1000 -e PGID=1000 -v "$TMP/config.json:/app/data/config.json:ro")
check_cmd "case 3: ready with a read-only config" ready "$C3"
procs_as "case 3 (PUID/PGID set)" "$C3" "$U:$G"
check "case 3: mosquitto still gets PUID stripped" "" \
    "$(dx "$C3" sh -c "tr '\\0' '\\n' < /proc/\$(pidof mosquitto)/environ | grep '^PUID='")"
publish "$C3" 6
check_cmd "case 3: ingestor stores with the read-only config" wait_until "$WAIT_S" ing_tx_is "$C3" 1
check_cmd "case 3: server reads it" wait_until "$WAIT_S" api_tx_is "$C3" 1
GEO=$(geo_put "$C3")
check "case 3: geo-filter save status" "409" "${GEO%% *}"
check_cmd "case 3: geo-filter message names the arrangement" sh -c "echo '$GEO' | grep -q 'geo filter not saved: config.json is read-only for the server'"
check "case 3: geo filter not applied" "" "$(dx "$C3" wget -qO- http://localhost:3000/api/config/geo-filter | grep -o '"polygon":\[\[')"
check "case 3: host config unchanged" "$CFG_SUM" "$(md5sum < "$TMP/config.json")"
check "case 3: config owner unchanged" "$(stat -c %u:%g "$TMP/config.json")" "$(as_root "$V3" 'stat -c %u:%g /app/data/config.json' -v "$TMP/config.json:/app/data/config.json:ro")"
docker rm -fv "$C3" > /dev/null
# Off mode with the same mount, for the record (behaviour is not changed here).
C3=$(start roconfig-off "$V3" -v "$TMP/config.json:/app/data/config.json:ro")
if ready "$C3"; then
    echo "# info: off mode geo-filter save with a read-only config mount -> $(geo_put "$C3" | cut -c1-120)"
    echo "# info: off mode /app/config.json afterwards: $(dx "$C3" stat -c %F /app/config.json); host config unchanged: $([ "$(md5sum < "$TMP/config.json")" = "$CFG_SUM" ] && echo yes || echo no)"
fi
docker rm -fv "$C3" > /dev/null
# The same refusal for a config.json that sits writable in the data volume
# (no :ro mount): in this mode the server never replaces it (#339 review,
# finding 2), whoever owns it and wherever the save would resolve to.
for cfg_owner in 0:0 1000:1000; do
    V3C=$(vol "inplace-${cfg_owner%%:*}")
    # The data dir has the config's owner, as on a bind mount the host user owns.
    as_root "$V3C" "printf '%s' '$CONFIG' > /app/data/config.json && chown $cfg_owner /app/data /app/data/config.json && chmod 644 /app/data/config.json"
    CFG_STAT=$(as_root "$V3C" 'md5sum < /app/data/config.json; stat -c "%i %u:%g %a" /app/data/config.json')
    C3=$(start "inplace-${cfg_owner%%:*}" "$V3C" "${ON[@]}")
    check_cmd "case 3 (in-volume config $cfg_owner): ready" ready "$C3"
    GEO=$(geo_put "$C3")
    check "case 3 (in-volume config $cfg_owner): geo-filter save status" "409" "${GEO%% *}"
    check "case 3 (in-volume config $cfg_owner): geo filter not applied" "" "$(dx "$C3" wget -qO- http://localhost:3000/api/config/geo-filter | grep -o '"polygon":\[\[')"
    check "case 3 (in-volume config $cfg_owner): file, inode, owner and mode unchanged" "$CFG_STAT" \
        "$(as_root "$V3C" 'md5sum < /app/data/config.json; stat -c "%i %u:%g %a" /app/data/config.json')"
    check "case 3 (in-volume config $cfg_owner): no temp file left" "" "$(as_root "$V3C" 'ls -A /app/data | grep -F config.json. || true')"
    docker rm -fv "$C3" > /dev/null
done
V3B=$(vol unreadable)
as_root "$V3B" "printf '%s' '$CONFIG' > /app/data/config.json && chmod 600 /app/data/config.json"
C3=$(start unreadable "$V3B" "${ON[@]}")
timeout 60 docker wait "$C3" > "$TMP/rc" 2>&1
check "case 3: unreadable config stops the start" "1" "$(cat "$TMP/rc")"
check_cmd "case 3: and says why" sh -c "docker logs '$C3' 2>&1 | grep -q 'config.json is not readable for $U:$G'"
docker rm -fv "$C3" > /dev/null

# ---------------------------------------------------------------------------
echo "# case 4: toggle off -> on -> off, in-place toggle, rollback"
V4=$(vol toggle)
as_root "$V4" "printf '%s' '$CONFIG' > /app/data/config.json"
C4=$(start toggle-off1 "$V4")
check_cmd "case 4 off: ready" ready "$C4"
procs_as "case 4 off" "$C4" "0:0"
ps_dump "case 4, off" "$C4"
publish "$C4" 7
check_cmd "case 4 off: packet stored and read" wait_until "$WAIT_S" api_tx_is "$C4" 1
docker rm -fv "$C4" > /dev/null
check "case 4 off: DB root-owned" "0:0" "$(owner "$V4" meshcore.db)"
# Record the owners before the first enable, as the docs say.
RECORD=$(docker run --rm --label "$LABEL" --pull never --network none -v "$V4:/app/data" \
    --entrypoint /usr/local/lib/corescope/nonroot.sh "$IMAGE" owners)
check_cmd "case 4: owners record lists the data dir" sh -c "printf '%s\n' '$RECORD' | grep -qx '0:0 755 \.'"
C4=$(start toggle-on "$V4" "${ON[@]}")
check_cmd "case 4 on: ready" ready "$C4"
procs_as "case 4 on" "$C4" "$U:$G"
ps_dump "case 4, on" "$C4"
publish "$C4" 8
check_cmd "case 4 on: packet stored and read" wait_until "$WAIT_S" api_tx_is "$C4" 2
docker rm -fv "$C4" > /dev/null
C4=$(start toggle-off2 "$V4")
check_cmd "case 4 off again: ready" ready "$C4"
procs_as "case 4 off again" "$C4" "0:0"
ps_dump "case 4, off again" "$C4"
check "case 4 off again: no ownership step ran" "0" "$(docker logs "$C4" 2>&1 | grep -c '^\[nonroot\] ownership')"
check "case 4 off again: group write and sticky bit taken back once" "1" \
    "$(docker logs "$C4" 2>&1 | grep -c '^\[nonroot\] the mode is off: removed group write and the sticky bit')"
check "case 4 off again: DB keeps the non-root owner" "$U:$G" "$(owner "$V4" meshcore.db)"
publish "$C4" 9
check_cmd "case 4 off again: packet stored and read" wait_until "$WAIT_S" api_tx_is "$C4" 3
# In place: the same container, switched through /app/data/.env.
as_root "$V4" "printf 'CORESCOPE_RUN_AS_NONROOT=1\nCORESCOPE_UID=$U\nCORESCOPE_GID=$G\n' > /app/data/.env"
check "case 4 in place: root-owned stats file left in /tmp" "0" "$(dx "$C4" stat -c %u /tmp/corescope-ingestor-stats.json)"
timeout 120 docker restart "$C4" > /dev/null
check_cmd "case 4 in place: ready" ready "$C4"
procs_as "case 4 in place via .env" "$C4" "$U:$G"
# Two packets: the stale root file from the off run already says 1.
publish "$C4" 10
publish "$C4" 11
check_cmd "case 4 in place: ingestor stats file rewritten as $U" wait_until "$WAIT_S" ing_tx_is "$C4" 2
check "case 4 in place: stats file owner" "$U" "$(dx "$C4" stat -c %u /tmp/corescope-ingestor-stats.json)"
check_cmd "case 4 in place: server reads 5 packets" wait_until "$WAIT_S" api_tx_is "$C4" 5
as_root "$V4" "rm /app/data/.env"
timeout 120 docker restart "$C4" > /dev/null
check_cmd "case 4 in place off: ready" ready "$C4"
procs_as "case 4 in place off" "$C4" "0:0"
timeout 60 docker stop "$C4" > /dev/null
ROLLBACK=$(printf '%s\n' "$RECORD" | docker run --rm -i --label "$LABEL" --pull never --network none --volumes-from "$C4" \
    --entrypoint /usr/local/lib/corescope/nonroot.sh "$IMAGE" restore-owners 2>&1)
check "case 4 rollback: restore-owners exit status" "0" "$?"
echo "# rollback: $ROLLBACK"
while read -r own mode name; do
    check "case 4 rollback: $name back to $own $mode" "$own $mode" "$(as_root "$V4" "stat -c '%u:%g %a' '/app/data/$name'")"
done <<< "$RECORD"
check "case 4 rollback: config untouched" "0:0" "$(owner "$V4" config.json)"
NR_BEFORE=$(docker logs "$C4" 2>&1 | grep -c '^\[nonroot\]')
docker start "$C4" > /dev/null
check_cmd "case 4 after rollback: ready" ready "$C4"
check "case 4 after rollback: no new [nonroot] line" "$NR_BEFORE" "$(docker logs "$C4" 2>&1 | grep -c '^\[nonroot\]')"
docker rm -fv "$C4" > /dev/null

# ---------------------------------------------------------------------------
echo "# case 5: invalid settings fail fast and change nothing"
V5=$(vol invalid)
as_root "$V5" "printf '%s' '$CONFIG' > /app/data/config.json && : > /app/data/meshcore.db"
BEFORE5=$(tree "$V5")
n=0
for args in "CORESCOPE_RUN_AS_NONROOT=yes CORESCOPE_UID=$U CORESCOPE_GID=$G" \
    "CORESCOPE_RUN_AS_NONROOT=1 CORESCOPE_GID=$G" \
    "CORESCOPE_RUN_AS_NONROOT=1 CORESCOPE_UID=0 CORESCOPE_GID=$G" \
    "CORESCOPE_RUN_AS_NONROOT=1 CORESCOPE_UID=101 CORESCOPE_GID=$G" \
    "CORESCOPE_RUN_AS_NONROOT=1 CORESCOPE_UID=$U CORESCOPE_GID=102"; do
    n=$((n + 1))
    env=()
    for a in $args; do env+=(-e "$a"); done
    C5=$(start "invalid$n" "$V5" "${env[@]}")
    timeout 60 docker wait "$C5" > "$TMP/rc" 2>&1
    check "case 5 ($args): exit status" "1" "$(cat "$TMP/rc")"
    check_cmd "case 5 ($args): clear message" sh -c "docker logs '$C5' 2>&1 | grep -q '^\[nonroot\] ERROR: '"
    docker rm -fv "$C5" > /dev/null
done
check "case 5: volume unchanged" "$BEFORE5" "$(tree "$V5")"

# ---------------------------------------------------------------------------
echo "# case 6: the services' user cannot change what root reads"
V6=$(vol access)
as_root "$V6" "printf '%s' '$CONFIG' > /app/data/config.json && echo '{}' > /app/data/theme.json &&
    chown 1000:1000 /app/data/theme.json"
C6=$(start access "$V6" "${ON[@]}")
check_cmd "case 6: ready" ready "$C6"
ROOT_FILES=$(as_root "$V6" 'cd /app/data && md5sum config.json theme.json && stat -c "%n %i %u:%g %a" config.json theme.json')
svc_try() { # LABEL SCRIPT — must be refused for the services' user
    if dx -u "$U:$G" "$C6" sh -c "cd /app/data && $2" > /dev/null 2>&1; then check "case 6: $1 refused" refused allowed
    else check "case 6: $1 refused" refused refused; fi
}
for f in config.json theme.json; do
    svc_try "$f write" "echo x >> $f"
    svc_try "$f remove" "rm -f $f && test ! -e $f"
    svc_try "$f replace by rename" "echo '{}' > new.json && mv -f new.json $f"
    svc_try "$f replace by symlink" "ln -sfn /etc/hostname $f"
done
svc_try "data dir mode change" "chmod -t /app/data"
check "case 6: root-read files unchanged" "$ROOT_FILES" \
    "$(as_root "$V6" 'cd /app/data && md5sum config.json theme.json && stat -c "%n %i %u:%g %a" config.json theme.json')"
# A missing .env can be created (the directory must stay writable for SQLite);
# the next start must refuse it before root reads it.
check_cmd "case 6: services can create a missing .env" dx -u "$U:$G" "$C6" sh -c \
    'printf "CORESCOPE_RUN_AS_NONROOT=0\ntouch /app/data/ran-as-root\n" > /app/data/.env'
timeout 120 docker restart "$C6" > /dev/null
timeout 60 docker wait "$C6" > "$TMP/rc" 2>&1
check "case 6: next start refused" "1" "$(cat "$TMP/rc")"
check_cmd "case 6: and says why" sh -c "docker logs '$C6' 2>&1 | grep -q '^\[nonroot\] ERROR: .*\.env'"
check "case 6: .env not sourced by root" "absent" "$(as_root "$V6" '[ -e /app/data/ran-as-root ] && echo present || echo absent')"
as_root "$V6" "rm /app/data/.env"
docker start "$C6" > /dev/null
check_cmd "case 6: ready again after the operator removed it" ready "$C6"
procs_as "case 6 after recovery" "$C6" "$U:$G"
# Admin commands: root is refused and writes nothing; the services' user works.
PUBKEY=$(printf 'c0ffee39%.0s' 1 2 3 4 5 6 7 8)
dx "$C6" /app/corescope-ingestor -config /app/config.json admin delete-client-rx --pubkey "$PUBKEY" --wait 5s > "$TMP/admin" 2>&1
check "case 6: root admin delete exit status" "1" "$?"
check_cmd "case 6: root admin delete says why" grep -q 'refusing to run as root' "$TMP/admin"
check "case 6: root admin delete wrote no request" "" \
    "$(as_root "$V6" 'ls /app/data/client-rx-deletes 2>/dev/null | grep "^request-" || true')"
dx -u "$U:$G" "$C6" /app/corescope-ingestor -config /app/config.json admin delete-client-rx --pubkey "$PUBKEY" --wait 60s > "$TMP/admin" 2>&1
check "case 6: admin delete as the services' user exit status" "0" "$?"
check_cmd "case 6: admin delete as the services' user ran" grep -q '^deleted client_receptions=' "$TMP/admin"
docker rm -fv "$C6" > /dev/null

echo "1..$CHECKS"
if [ "$FAILS" -eq 0 ]; then
    echo "RESULT: PASS ($CHECKS checks)"
    exit 0
fi
echo "RESULT: FAIL ($FAILS of $CHECKS checks failed)"
exit 1
