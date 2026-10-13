#!/usr/bin/env bash
# scripts/test-nonroot-entrypoint.sh — CI test of the container start path of
# the opt-in non-root mode (#339) with supervisord stubbed.
#
# Runs the real docker/entrypoint-go.sh and docker/nonroot.sh as root in the
# runtime image's pinned Alpine base (parsed from Dockerfile.go), with the
# four shipped supervisor configs and a stub supervisord that prints what it
# was started with. No Go build, no network inside the containers; every
# volume is labelled, created here and removed on exit.
#
# What it pins (each case fails if the property breaks):
#   off     unset/empty/0/false x the DISABLE_* combinations start the shipped
#           config exactly as before: no [nonroot] output, .env sourced, config
#           linked, volume untouched
#   on      the generated config comes from the selected one; the data dir
#           keeps its owner, gets the services' group, group write and the
#           sticky bit; only the services' own paths change owner, only when
#           the owner differs (second start: "0 of N")
#   access  a process running as CORESCOPE_UID:CORESCOPE_GID cannot modify,
#           replace, remove or re-mode .env/config.json/theme.json, whether the
#           host user or root owns them, nor change the data dir
#   gate    a root-consumed file the services could have written (their owner,
#           group-writable for them, a symlink, a hard link, created after the
#           enable) stops the start with [nonroot] ERROR before anything is
#           sourced or linked; also with the setting removed afterwards, and
#           on a volume without the marker whose .env alone asks for the mode
#   queue   requests in the queue directories that the services did not write
#           are set aside, never handed to the services
#   record  owners/restore-owners restore mixed owners and the data dir mode
#           exactly; set-owner is the coarse fallback; both refuse while an
#           untrusted root-consumed file is present
#
# usage: bash scripts/test-nonroot-entrypoint.sh
# Exit: 0 all checks passed, 1 a check failed, 2 setup error.
# In-container scripts are single-quoted on purpose.
# shellcheck disable=SC2016,SC2317
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASE=$(sed -n 's/^FROM \(alpine:[^ ]*\)$/\1/p' "$ROOT/Dockerfile.go" | tail -1)
[ -n "$BASE" ] || { echo "cannot find the runtime base image in Dockerfile.go" >&2; exit 2; }
docker image inspect "$BASE" > /dev/null 2>&1 || docker pull -q "$BASE" > /dev/null ||
    { echo "cannot pull $BASE" >&2; exit 2; }

RUN_ID="nonroot-ep-$(date +%Y%m%d%H%M%S)-$$"
LABEL="com.corescope.test.run=$RUN_ID"
TMP=$(mktemp -d)
U=10001
G=10001
H=1000 # the host user who owns the bind-mounted data dir
ON=(-e CORESCOPE_RUN_AS_NONROOT=1 -e "CORESCOPE_UID=$U" -e "CORESCOPE_GID=$G")
CHECKS=0
FAILS=0

cleanup() {
    local v
    for v in $(docker ps -aq --filter "label=$LABEL"); do docker rm -fv "$v" > /dev/null 2>&1; done
    for v in $(docker volume ls -q --filter "label=$LABEL"); do docker volume rm "$v" > /dev/null 2>&1; done
    rm -rf "$TMP"
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
        echo "#   actual:   $3" | head -20
    fi
}
has() { if printf '%s\n' "$2" | grep -qE -- "$3"; then check "$1" yes yes; else check "$1" "output matching /$3/" "$2"; fi; }
hasnt() { if printf '%s\n' "$2" | grep -qE -- "$3"; then check "$1" "no line matching /$3/" "$(printf '%s\n' "$2" | grep -E -- "$3")"; else check "$1" yes yes; fi; }

# The image layout the entrypoint expects, from the files under test.
mkdir -p "$TMP/conf"
install -m 755 "$ROOT/docker/entrypoint-go.sh" "$TMP/entrypoint.sh"
install -m 755 "$ROOT/docker/nonroot.sh" "$TMP/nonroot.sh"
for c in supervisord supervisord-no-mosquitto supervisord-no-caddy supervisord-no-mosquitto-no-caddy; do
    cp "$ROOT/docker/${c/supervisord/supervisord-go}.conf" "$TMP/conf/$c.conf"
done
cat > "$TMP/supervisord" << 'EOF'
#!/bin/sh
echo "argv: $*"
echo "conf: $(md5sum < "$2" | cut -d' ' -f1)"
env | grep -E '^(CORESCOPE_|FROM_ENV_FILE=)' | LC_ALL=C sort | sed 's/^/env: /'
if [ -f /hook.sh ]; then sh /hook.sh; fi
EOF
chmod 755 "$TMP/supervisord"
MOUNTS=(-v "$TMP/entrypoint.sh:/entrypoint.sh:ro" -v "$TMP/nonroot.sh:/usr/local/lib/corescope/nonroot.sh:ro"
    -v "$TMP/conf:/etc/supervisor/conf.d:ro" -v "$TMP/supervisord:/usr/bin/supervisord:ro")

vol() { docker volume create --label "$LABEL" "cs-$RUN_ID-$1" > /dev/null && echo "cs-$RUN_ID-$1"; }
# as_root VOL SCRIPT [docker args...]: SCRIPT as root on the volume.
as_root() {
    local v=$1 s=$2
    shift 2
    docker run --rm --label "$LABEL" --network none -v "$v:/app/data" "$@" --entrypoint sh "$BASE" -c "$s"
}
# as_svc VOL SCRIPT: SCRIPT as CORESCOPE_UID:CORESCOPE_GID on the volume.
as_svc() { docker run --rm --label "$LABEL" --network none --user "$U:$G" -v "$1:/app/data" --entrypoint sh "$BASE" -c "$2"; }
# start VOL [docker args...]: one container start through the entrypoint. The
# wrapper reports what the root side did after the entrypoint returned or
# the stub supervisord exited. Sets OUT (stdout+stderr) and RC.
start() {
    local v=$1
    shift
    OUT=$(docker run --rm --label "$LABEL" --network none -v "$v:/app/data" "${MOUNTS[@]}" "$@" \
        --entrypoint sh "$BASE" -c '/entrypoint.sh; rc=$?
            for f in /app/config.json /app/theme.json; do if [ -L "$f" ]; then echo "linked: $f"; fi; done
            exit $rc' 2>&1)
    RC=$?
}
# helper VOL ARGS...: the helper itself as root (rollback commands).
helper() {
    local v=$1
    shift
    docker run --rm -i --label "$LABEL" --network none -v "$v:/app/data" "${MOUNTS[@]}" \
        --entrypoint /usr/local/lib/corescope/nonroot.sh "$BASE" "$@"
}
tree() { as_root "$1" 'cd /app/data && find . | LC_ALL=C sort | while read -r f; do stat -c "%n %u:%g %a %h %F" "$f"; done'; }
stat_of() { as_root "$1" "cd /app/data && stat -c '%u:%g %a' $2 2>&1"; }

# layout VOL: a bind-mount-like data dir owned by the host user
# with the user's own config, theme and .env, plus root-owned service files
# left by an earlier root run.
layout() {
    as_root "$1" "chown $H:$H /app/data && chmod 775 /app/data && cd /app/data &&
        printf '{\"apiKey\":\"k\"}' > config.json && echo '{}' > theme.json &&
        echo FROM_ENV_FILE=yes > .env && chown $H:$H config.json theme.json .env &&
        chmod 644 config.json theme.json .env &&
        : > meshcore.db && : > meshcore.db-wal && : > ping_scores_history.db &&
        mkdir notes && echo keep > notes/keep.txt && chown -R $H:$H notes"
}

echo "# base image: $BASE"

# ---------------------------------------------------------------------------
echo "# off: the shipped start, unchanged"
V=$(vol off)
layout "$V"
BEFORE=$(tree "$V")
for combo in none DISABLE_MOSQUITTO DISABLE_CADDY "DISABLE_MOSQUITTO DISABLE_CADDY"; do
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
        start "$V" "${dis[@]}" "${env[@]}" -e "CORESCOPE_UID=$U" -e "CORESCOPE_GID=$G"
        check "$tag: exit status" 0 "$RC"
        has "$tag: shipped supervisor config" "$OUT" "^argv: -c /etc/supervisor/conf.d/$conf$"
        hasnt "$tag: helper silent" "$OUT" '\[nonroot\]'
        has "$tag: .env sourced" "$OUT" '^env: FROM_ENV_FILE=yes$'
        has "$tag: config linked" "$OUT" '^linked: /app/config.json$'
    done
    start "$V" "${dis[@]}" "${ON[@]}"
    want=$(docker run --rm --network none "${MOUNTS[@]}" --entrypoint /usr/local/lib/corescope/nonroot.sh "$BASE" \
        render-conf "/etc/supervisor/conf.d/$conf" corescope | md5sum | cut -d' ' -f1)
    has "on ($combo): generated config" "$OUT" '^argv: -c /run/corescope/supervisord.conf$'
    has "on ($combo): generated from $conf" "$OUT" "^conf: $want$"
    # Back to the off state for the next combination (a rollback, by hand).
    as_root "$V" "cd /app/data && chown $H:$H . && chmod 775 . && rm .corescope-nonroot &&
        chown 0:0 meshcore.db meshcore.db-wal ping_scores_history.db"
done
check "off: the volume is untouched by every off start" "$BEFORE" "$(tree "$V")"
start "$V" -e CORESCOPE_RUN_AS_NONROOT=yes
check "invalid setting: exit status" 1 "$RC"
has "invalid setting: clear message" "$OUT" '^\[nonroot\] ERROR: '

# ---------------------------------------------------------------------------
echo "# on: first and second start"
V=$(vol on)
layout "$V"
OWN_BEFORE=$(tree "$V" | grep -E '^\./(config\.json|theme\.json|\.env|notes|notes/keep\.txt) ')
start "$V" "${ON[@]}"
check "on: exit status" 0 "$RC"
has "on: generated config" "$OUT" '^argv: -c /run/corescope/supervisord.conf$'
check "on: data dir keeps its owner, gets the services' group, group write and the sticky bit" \
    "$H:$G 1775" "$(stat_of "$V" .)"
for f in meshcore.db meshcore.db-wal ping_scores_history.db; do
    check "on: $f handed to the services" "$U:$G 644" "$(stat_of "$V" "$f")"
done
check "on: the user's own files keep owner and mode" "$OWN_BEFORE" \
    "$(tree "$V" | grep -E '^\./(config\.json|theme\.json|\.env|notes|notes/keep\.txt) ')"
has "on: ownership reported" "$OUT" '^\[nonroot\] ownership: 3 of [0-9]+ path'
check "on: root-owned marker records the IDs" "0:0 644 $U:$G" \
    "$(as_root "$V" 'cd /app/data && echo "$(stat -c "%u:%g %a" .corescope-nonroot) $(cat .corescope-nonroot)"')"
CT1=$(as_root "$V" 'stat -c "%n %z" /app/data /app/data/meshcore.db /app/data/ping_scores_history.db')
start "$V" "${ON[@]}"
check "on, second start: exit status" 0 "$RC"
has "on, second start: nothing to change" "$OUT" '^\[nonroot\] ownership: 0 of [0-9]+ path'
check "on, second start: no chown or chmod touched the listed paths (ctime)" "$CT1" \
    "$(as_root "$V" 'stat -c "%n %z" /app/data /app/data/meshcore.db /app/data/ping_scores_history.db')"
start "$V" -e CORESCOPE_RUN_AS_NONROOT=1 -e CORESCOPE_UID=$H -e "CORESCOPE_GID=$G"
check "on with the data dir owner as CORESCOPE_UID: refused" 1 "$RC"
has "on with the data dir owner as CORESCOPE_UID: says why" "$OUT" '^\[nonroot\] ERROR: .*owns the data directory'

# ---------------------------------------------------------------------------
echo "# access: the services cannot change what root reads"
cat > "$TMP/hook.sh" << EOF
cd /app/data
svc() { su -s /bin/sh corescope -c "\$1" > /dev/null 2>&1; }
try() { if svc "\$2"; then echo "attempt \$1: allowed"; else echo "attempt \$1: denied"; fi; }
for f in .env config.json theme.json; do
    try "\$f append" "echo x >> \$f"
    try "\$f truncate" ": > \$f"
    try "\$f chmod" "chmod 666 \$f"
    try "\$f remove" "rm -f \$f && test ! -e \$f"
    try "\$f rename away" "mv \$f moved.\$f"
    try "\$f replace by rename" "echo x > new.\$f && mv -f new.\$f \$f"
    try "\$f replace by symlink" "ln -sfn /etc/hostname \$f"
done
try "control: runs as the services' user" "[ \\\$(id -u):\\\$(id -g) = $U:$G ]"
try "data dir chmod" "chmod 777 /app/data"
try "data dir chgrp" "chgrp $U /app/data"
try "control: create and remove an own file" "echo x > own.tmp && rm own.tmp"
try "control: write the database" "echo x >> meshcore.db"
EOF
for owner in "$H:$H" "0:0"; do
    V=$(vol "access-${owner%%:*}")
    layout "$V"
    as_root "$V" "chown $owner config.json theme.json .env"
    SUMS=$(as_root "$V" 'cd /app/data && md5sum .env config.json theme.json && stat -c "%n %i %u:%g %a" .env config.json theme.json')
    start "$V" "${ON[@]}" -v "$TMP/hook.sh:/hook.sh:ro"
    check "access ($owner originals): exit status" 0 "$RC"
    for f in .env config.json theme.json; do
        for op in append truncate chmod remove "rename away" "replace by rename" "replace by symlink"; do
            has "access ($owner originals): $f $op denied" "$OUT" "^attempt $f $op: denied$"
        done
    done
    has "access ($owner originals): attempts run as $U:$G" "$OUT" "^attempt control: runs as the services' user: allowed$"
    has "access ($owner originals): data dir chmod denied" "$OUT" '^attempt data dir chmod: denied$'
    has "access ($owner originals): data dir chgrp denied" "$OUT" '^attempt data dir chgrp: denied$'
    has "access ($owner originals): services can still create and remove their own files" "$OUT" '^attempt control: create and remove an own file: allowed$'
    has "access ($owner originals): services can still write the database" "$OUT" '^attempt control: write the database: allowed$'
    check "access ($owner originals): files byte- and inode-identical" "$SUMS" \
        "$(as_root "$V" 'cd /app/data && md5sum .env config.json theme.json && stat -c "%n %i %u:%g %a" .env config.json theme.json')"
    start "$V" "${ON[@]}"
    check "access ($owner originals): the next start still passes the check" 0 "$RC"
done

# ---------------------------------------------------------------------------
echo "# gate: a file the services could have written is never used by root"
PAYLOAD='touch /app/data/ran-as-root'
# gate_refused VOL LABEL [docker args...]: expect the next start to stop
# before sourcing or linking anything.
gate_refused() {
    local v=$1 label=$2
    shift 2
    start "$v" "$@"
    check "gate ($label): start refused" 1 "$RC"
    has "gate ($label): [nonroot] ERROR" "$OUT" '^\[nonroot\] ERROR: '
    hasnt "gate ($label): supervisord not started" "$OUT" '^argv: '
    hasnt "gate ($label): nothing linked" "$OUT" '^linked: '
    check "gate ($label): .env not sourced" "absent" "$(as_root "$v" '[ -e /app/data/ran-as-root ] && echo present || echo absent')"
}
# gate_case LABEL ROOT_SCRIPT [docker args...]: prepare an enabled volume,
# tamper as ROOT_SCRIPT says, then expect gate_refused.
gate_case() {
    local label=$1 prep=$2
    shift 2
    local v
    v=$(vol "gate-$CHECKS")
    layout "$v"
    start "$v" "${ON[@]}"
    [ "$RC" = 0 ] || { check "gate ($label): setup start" 0 "$RC"; return; }
    as_root "$v" "cd /app/data && $prep"
    gate_refused "$v" "$label" "$@"
}
gate_case ".env owned by the services" "echo '$PAYLOAD' >> .env && chown $U:$G .env" "${ON[@]}"
has "gate (.env owned by the services): names the services' user" "$OUT" "ERROR: /app/data/.env is owned by $U, the services' user"
gate_case ".env created after the enable" "rm .env && echo '$PAYLOAD' > .env && chown $U:$G .env" "${ON[@]}"
gate_case ".env group-writable for the services" "echo '$PAYLOAD' >> .env && chgrp $G .env && chmod 664 .env" "${ON[@]}"
gate_case ".env world-writable" "chmod 666 .env" "${ON[@]}"
gate_case ".env a symlink" "echo '$PAYLOAD' > notes/x && mv .env notes/env && ln -s notes/x .env" "${ON[@]}"
gate_case ".env hard-linked" "echo '$PAYLOAD' >> .env && ln .env notes/env-link" "${ON[@]}"
gate_case "config.json owned by the services" "chown $U:$G config.json" "${ON[@]}"
gate_case "config.json a symlink" "mv config.json notes/c.json && ln -s notes/c.json config.json" "${ON[@]}"
gate_case "theme.json owned by the services" "chown $U theme.json" "${ON[@]}"
gate_case "theme.json not a regular file" "rm theme.json && mkfifo theme.json" "${ON[@]}"
gate_case "setting only in .env, .env owned by the services" \
    "printf 'CORESCOPE_RUN_AS_NONROOT=1\nCORESCOPE_UID=$U\nCORESCOPE_GID=$G\n$PAYLOAD\n' > .env && chown $U:$G .env"
gate_case "setting removed afterwards, service-created .env" "rm .env && echo '$PAYLOAD' > .env && chown $U:$G .env"
gate_case "setting in .env not written as a plain number" \
    "printf 'CORESCOPE_RUN_AS_NONROOT=1\nCORESCOPE_UID=\$(echo $U)\nCORESCOPE_GID=$G\n' > .env"
gate_case "data dir handed to the services" "chown $U /app/data" "${ON[@]}"
gate_case "marker not written by root" "chown $U:$G .corescope-nonroot"
gate_case ".env owned by neither root nor the data dir's owner" "echo '$PAYLOAD' >> .env && chown 2000:2000 .env" "${ON[@]}"
# No marker yet, the setting only in .env: the mention in .env alone must run
# the check before root sources .env. This is the only trigger on a volume
# from the earlier draft (eb49596f), which never wrote the marker.
NR_ENV="printf 'CORESCOPE_RUN_AS_NONROOT=1\nCORESCOPE_UID=$U\nCORESCOPE_GID=$G\n$PAYLOAD\n' > .env"
for c in "group-writable .env|$NR_ENV && chgrp $G .env && chmod 664 .env" \
    "earlier draft: data dir and .env owned by the services|$NR_ENV && chown $U:$G . .env && chmod 755 ."; do
    V=$(vol "gate-$CHECKS")
    layout "$V"
    as_root "$V" "cd /app/data && ${c#*|}"
    check "gate (no marker, ${c%%|*}): volume has no marker" "absent" \
        "$(as_root "$V" '[ -e /app/data/.corescope-nonroot ] && echo present || echo absent')"
    gate_refused "$V" "no marker, setting only in .env, ${c%%|*}"
done

# A read-only mount cannot be changed or replaced from inside the container,
# whoever owns it on the host.
V=$(vol gate-ro-mount)
layout "$V"
as_root "$V" "cd /app/data && chown 0:0 . .env theme.json"
printf '{"apiKey":"k"}' > "$TMP/ro-config.json"
chmod 644 "$TMP/ro-config.json"
start "$V" "${ON[@]}" -v "$TMP/ro-config.json:/app/data/config.json:ro"
check "read-only config mount owned by $(id -u) (not root, not the data dir owner): accepted" 0 "$RC"
has "read-only config mount: linked" "$OUT" '^linked: /app/config.json$'

# The setting removed, nothing tampered: root runs everything again, and the
# check keeps running until the rollback below clears the mark.
V=$(vol gate-clean-off)
layout "$V"
start "$V" "${ON[@]}"
start "$V"
check "off after on, clean volume: exit status" 0 "$RC"
has "off after on, clean volume: shipped config" "$OUT" '^argv: -c /etc/supervisor/conf.d/supervisord.conf$'
has "off after on, clean volume: .env sourced" "$OUT" '^env: FROM_ENV_FILE=yes$'
has "off after on: says it took back group write and the sticky bit" "$OUT" '^\[nonroot\] the mode is off: removed group write and the sticky bit'
check "off after on: data dir no longer group-writable or sticky (fs.protected_regular)" "$H:$G 755" "$(stat_of "$V" .)"
start "$V"
hasnt "off after on, second start: nothing left to change" "$OUT" '\[nonroot\]'
start "$V" "${ON[@]}"
check "on again: data dir set up again" "$H:$G 1775" "$(stat_of "$V" .)"

# ---------------------------------------------------------------------------
echo "# queue: requests the services did not write are set aside"
V=$(vol queue)
layout "$V"
as_root "$V" "cd /app/data && mkdir client-rx-deletes prune-requests channel-proposal-requests mbcap-snapshot &&
    echo '{}' > client-rx-deletes/request-root.json && chmod 600 client-rx-deletes/request-root.json &&
    echo '{}' > prune-requests/request-root.json && echo '{}' > prune-requests/result-old.json &&
    echo '{}' > channel-proposal-requests/cmd-root.json && echo '{}' > mbcap-snapshot/snapshot.json &&
    echo '{}' > prune-requests/request-own.json && chown $U:$G prune-requests/request-own.json"
start "$V" "${ON[@]}"
check "queue: exit status" 0 "$RC"
for q in client-rx-deletes prune-requests channel-proposal-requests mbcap-snapshot; do
    check "queue: $q handed to the services" "$U:$G 755" "$(stat_of "$V" "$q")"
done
for r in client-rx-deletes/request-root.json prune-requests/request-root.json channel-proposal-requests/cmd-root.json; do
    check "queue: root's $r no longer pending" "absent" "$(as_root "$V" "[ -e /app/data/$r ] && echo present || echo absent")"
    check "queue: root's $r kept aside, still root's" "0:0" "$(stat_of "$V" "$r.held" | cut -d' ' -f1)"
    has "queue: $r reported" "$OUT" "^\[nonroot\] WARNING: .*$r"
done
check "queue: the services' own request stays pending" "$U:$G" "$(stat_of "$V" prune-requests/request-own.json | cut -d' ' -f1)"
check "queue: other entries untouched" "0:0 644|0:0 644" \
    "$(stat_of "$V" prune-requests/result-old.json)|$(stat_of "$V" mbcap-snapshot/snapshot.json)"
start "$V" "${ON[@]}"
check "queue: held requests stay held on the next start" "0:0" "$(stat_of "$V" client-rx-deletes/request-root.json.held | cut -d' ' -f1)"
hasnt "queue: and are not reported again" "$OUT" 'WARNING: .*request-root'

# ---------------------------------------------------------------------------
echo "# record: rollback restores mixed owners exactly"
V=$(vol record)
layout "$V"
as_root "$V" "cd /app/data && chown $H:$H meshcore.db && chmod 664 meshcore.db && mkdir prune-requests &&
    echo '{}' > prune-requests/result-1.json && chmod 700 prune-requests &&
    printf salt > client-rx-delete.salt && chmod 600 client-rx-delete.salt"
RECORD=$(helper "$V" owners)
has "record: lists the data dir with its mode" "$RECORD" "^$H:$H 775 \.$"
has "record: lists queue entries" "$RECORD" '^0:0 [0-7]+ prune-requests/result-1\.json$'
LISTED_BEFORE=$(tree "$V" | grep -vE '^\./(config\.json|theme\.json|\.env|notes|notes/keep\.txt) ')
start "$V" "${ON[@]}"
check "record: on start" 0 "$RC"
as_svc "$V" ': > /app/data/meshcore.db-shm'
printf '%s\n' "$RECORD" | helper "$V" restore-owners > "$TMP/restore.out" 2>&1
check "record: restore-owners exit status" 0 "$?"
check "record: every recorded path has its recorded owner and mode again" "$LISTED_BEFORE" \
    "$(tree "$V" | grep -vE '^\./(config\.json|theme\.json|\.env|notes|notes/keep\.txt|meshcore\.db-shm) ')"
check "record: a path created while on goes to root" "0:0" "$(stat_of "$V" meshcore.db-shm | cut -d' ' -f1)"
start "$V"
check "record: off start after the rollback" 0 "$RC"
hasnt "record: no check left after the rollback" "$OUT" '\[nonroot\]'
# A record that names anything outside the list is refused as a whole.
for bad in '0:0 755 ../etc' '0:0 644 notes/keep.txt' '0:0 644 /etc/passwd' "0:0 644 config.json" '0 644 meshcore.db'; do
    out=$(printf '%s\n%s\n0:0 644 meshcore.db\n' "$H:$H 775 ." "$bad" | helper "$V" restore-owners 2>&1)
    rc=$?
    check "record: bad line '$bad' refused" 1 "$rc"
    has "record: bad line '$bad' says why" "$out" '^\[nonroot\] ERROR: '
done
check "record: refused records changed nothing" "$H:$H 664|$H:$H 775" "$(stat_of "$V" meshcore.db)|$(stat_of "$V" .)"
# Rollback refuses while a root-consumed file is not trustworthy.
V=$(vol record-untrusted)
layout "$V"
start "$V" "${ON[@]}"
as_root "$V" "cd /app/data && rm .env && echo '$PAYLOAD' > .env && chown $U:$G .env"
out=$(echo "$H:$H 775 ." | helper "$V" restore-owners 2>&1)
check "record: restore-owners refuses with an untrusted .env" 1 "$?"
has "record: and names it" "$out" 'ERROR: .*\.env'
out=$(helper "$V" set-owner 0:0 2>&1)
check "record: set-owner refuses with an untrusted .env" 1 "$?"
check "record: refusals left the data dir marked" "$H:$G 1775" "$(stat_of "$V" .)"
as_root "$V" "rm /app/data/.env"
out=$(helper "$V" set-owner 0:0 2>&1)
check "record: set-owner after the operator removed it" 0 "$?"
check "record: set-owner gives the listed paths to 0:0" "0:0 0:0" \
    "$(stat_of "$V" . | cut -d' ' -f1) $(stat_of "$V" meshcore.db | cut -d' ' -f1)"
check "record: set-owner clears the group write and sticky bits" "755" "$(stat_of "$V" . | cut -d' ' -f2)"

echo "1..$CHECKS"
if [ "$FAILS" -eq 0 ]; then
    echo "RESULT: PASS ($CHECKS checks)"
    exit 0
fi
echo "RESULT: FAIL ($FAILS of $CHECKS checks failed)"
exit 1
