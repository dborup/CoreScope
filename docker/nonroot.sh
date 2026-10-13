#!/bin/sh
# Opt-in non-root mode for corescope-server and corescope-ingestor (#339).
#
# entrypoint-go.sh calls `setup` only when CORESCOPE_RUN_AS_NONROOT is set to
# something other than unset, empty, 0 or false; with the setting off this
# file is never executed and the container starts exactly as before.
#
#   setup BASE_CONF      as root: create the run account, hand the Go
#                        services' own files in /app/data to it, write a
#                        supervisor config that runs only the two Go programs
#                        as that account, and print the config's path
#   set-owner UID:GID    as root, container stopped: give the same list of
#                        paths to UID:GID (rollback, e.g. 0:0)
#   render-conf BASE_CONF USER   print BASE_CONF with user=USER added to the
#                        two Go programs (Mosquitto and Caddy untouched)
#   validate             check the settings, print UID:GID
#   paths                print the migration list ("f name" / "d name")
#
# Ownership is changed only on the paths below, only where it differs, never
# recursively and never through a symlink or onto a hard-linked file.
set -eu

DATA_DIR=/app/data
RUN_USER=corescope
GENERATED_CONF=/run/corescope/supervisord.conf
STATS_FILE=/tmp/corescope-ingestor-stats.json
PASSWD=${NONROOT_PASSWD:-/etc/passwd}
GROUP=${NONROOT_GROUP:-/etc/group}

# What the Go services create next to the database. Kept in sync with the Go
# constants by scripts/test-nonroot-helper.sh. Queue directories are flat:
# their direct entries are included, nothing deeper.
DB_FILES="meshcore.db meshcore.db-wal meshcore.db-shm ping_scores_history.db ping_scores_history.db-wal ping_scores_history.db-shm"
SIDE_FILES="client-rx-delete.salt client-rx-delete-audit.jsonl"
QUEUE_DIRS="prune-requests mbcap-snapshot channel-proposal-requests client-rx-deletes"

log() { echo "[nonroot] $*" >&2; }
die() {
  echo "[nonroot] ERROR: $*" >&2
  exit 1
}

# A decimal ID in 1..2147483647 without a leading zero.
valid_id() {
  case "$1" in '' | 0* | *[!0-9]*) return 1 ;; esac
  [ "${#1}" -le 10 ] && [ "$1" -le 2147483647 ]
}

# name_by FILE COLUMN VALUE: name of the first entry whose COLUMN equals VALUE.
name_by() { awk -F: -v c="$2" -v v="$3" '$c == v { print $1; exit }' "$1"; }

validate() {
  case "${CORESCOPE_RUN_AS_NONROOT-}" in
    1 | true) ;;
    *) die "CORESCOPE_RUN_AS_NONROOT='${CORESCOPE_RUN_AS_NONROOT-}' is not a valid value; use 1 or true to enable, unset, 0 or false to disable" ;;
  esac
  uid=${CORESCOPE_UID-}
  gid=${CORESCOPE_GID-}
  valid_id "$uid" || die "CORESCOPE_UID='$uid' must be a non-zero numeric user ID (e.g. the host owner of the data directory: id -u)"
  valid_id "$gid" || die "CORESCOPE_GID='$gid' must be a non-zero numeric group ID (e.g. id -g)"
  owner=$(name_by "$PASSWD" 3 "$uid")
  if [ -n "$owner" ] && [ "$owner" != "$RUN_USER" ]; then
    die "CORESCOPE_UID=$uid already belongs to the image account '$owner'; choose an unused ID"
  fi
  [ "$gid" -ge 100 ] || die "CORESCOPE_GID=$gid is a system group ID (<100); choose 100 or higher"
  group=$(name_by "$GROUP" 3 "$gid")
  case "$group" in
    mosquitto | caddy) die "CORESCOPE_GID=$gid is the '$group' group of another service; choose another ID" ;;
  esac
}

paths() {
  for f in $DB_FILES $SIDE_FILES; do echo "f $f"; done
  for d in $QUEUE_DIRS; do echo "d $d"; done
}

render_conf() {
  awk -v user="$2" '
    { print }
    /^\[program:corescope-(ingestor|server)\][ \t]*$/ { print "user=" user; n++ }
    END { if (n != 2) exit 3 }
  ' "$1" || die "$1 does not define both [program:corescope-ingestor] and [program:corescope-server]"
}

# ensure_account UID GID: the passwd/group entries supervisor needs for user=.
ensure_account() {
  cur=$(awk -F: -v n="$RUN_USER" '$1 == n { print $3 ":" $4; exit }' "$PASSWD")
  [ "$cur" = "$1:$2" ] && return 0
  [ -z "$cur" ] || deluser "$RUN_USER" >&2 || true
  g=$(name_by "$GROUP" 3 "$2")
  if [ -z "$g" ]; then
    gname=$(name_by "$GROUP" 1 "$RUN_USER")
    [ -z "$gname" ] || delgroup "$RUN_USER" >&2 || true
    addgroup -g "$2" "$RUN_USER" >&2 || die "could not create group $2"
    g=$RUN_USER
  fi
  adduser -D -H -h /nonexistent -s /sbin/nologin -g CoreScope -G "$g" -u "$1" "$RUN_USER" >&2 ||
    die "could not create user $1"
}

checked=0
changed=0

# fix_owner PATH OWNER: chown PATH (itself only) when its owner differs.
fix_owner() {
  if [ -L "$1" ]; then
    log "WARNING: $1 is a symlink; left unchanged"
    return 0
  fi
  [ -e "$1" ] || return 0
  if [ -f "$1" ]; then
    if [ "$(stat -c %h "$1")" -gt 1 ]; then
      log "WARNING: $1 is hard-linked; left unchanged"
      return 0
    fi
  elif [ ! -d "$1" ]; then
    log "WARNING: $1 is not a regular file or directory; left unchanged"
    return 0
  fi
  checked=$((checked + 1))
  [ "$(stat -c %u:%g "$1")" = "$2" ] && return 0
  chown -h "$2" "$1" || die "could not change the owner of $1 (read-only data volume?)"
  changed=$((changed + 1))
}

migrate() {
  [ -d "$DATA_DIR" ] || die "$DATA_DIR does not exist"
  fix_owner "$DATA_DIR" "$1"
  for f in $DB_FILES $SIDE_FILES; do fix_owner "$DATA_DIR/$f" "$1"; done
  for d in $QUEUE_DIRS; do
    fix_owner "$DATA_DIR/$d" "$1"
    if [ ! -d "$DATA_DIR/$d" ] || [ -L "$DATA_DIR/$d" ]; then continue; fi
    for e in "$DATA_DIR/$d"/* "$DATA_DIR/$d"/.[!.]*; do
      [ -e "$e" ] || [ -L "$e" ] || continue
      fix_owner "$e" "$1"
    done
  done
}

as_user() { su -s /bin/sh "$RUN_USER" -c "$1"; }

setup() {
  [ "$(id -u)" = 0 ] || die "setup must run as root"
  validate
  base=$1
  ensure_account "$uid" "$gid"
  migrate "$uid:$gid"
  log "ownership: $changed of $checked path(s) in $DATA_DIR changed to $uid:$gid"

  # The services must be able to use what they did not get to own.
  as_user "test -w $DATA_DIR && test -x $DATA_DIR" ||
    die "$DATA_DIR is not writable for $uid:$gid after the ownership fix"
  for cfg in "$DATA_DIR/config.json" /app/config.json; do
    [ -e "$cfg" ] || continue
    as_user "test -r $cfg" ||
      die "$cfg is not readable for $uid:$gid; make it readable for that user or group on the host (CoreScope never changes config ownership)"
  done

  # A stats file left by a root run of this container (docker restart keeps
  # /tmp) would block the non-root ingestor; it is rewritten every second.
  if [ -z "${CORESCOPE_INGESTOR_STATS-}" ]; then
    for f in "$STATS_FILE" "$STATS_FILE.tmp"; do
      if [ -L "$f" ] || { [ -e "$f" ] && [ "$(stat -c %u "$f")" != "$uid" ]; }; then
        rm -f "$f"
      fi
    done
  fi

  mkdir -p "$(dirname "$GENERATED_CONF")"
  render_conf "$base" "$RUN_USER" > "$GENERATED_CONF.tmp"
  mv "$GENERATED_CONF.tmp" "$GENERATED_CONF"
  log "corescope-server and corescope-ingestor run as $uid:$gid; Mosquitto and Caddy unchanged"
  echo "$GENERATED_CONF"
}

set_owner() {
  [ "$(id -u)" = 0 ] || die "set-owner must run as root"
  case "$1" in
    *:*) ;;
    *) die "usage: set-owner UID:GID" ;;
  esac
  for id in "${1%%:*}" "${1#*:}"; do
    [ "$id" = 0 ] || valid_id "$id" || die "invalid ID '$id'"
  done
  migrate "$1"
  log "ownership: $changed of $checked path(s) in $DATA_DIR changed to $1"
}

cmd=${1-}
[ $# -eq 0 ] || shift
case "$cmd" in
  setup) [ $# -eq 1 ] || die "usage: $0 setup BASE_CONF"; setup "$1" ;;
  set-owner) [ $# -eq 1 ] || die "usage: $0 set-owner UID:GID"; set_owner "$1" ;;
  render-conf) [ $# -eq 2 ] || die "usage: $0 render-conf BASE_CONF USER"; render_conf "$1" "$2" ;;
  validate) validate; echo "$uid:$gid" ;;
  paths) paths ;;
  *) die "usage: $0 setup|set-owner|render-conf|validate|paths" ;;
esac
