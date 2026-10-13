#!/bin/sh
# Opt-in non-root mode for corescope-server and corescope-ingestor (#339).
#
# entrypoint-go.sh calls `preflight` before it reads anything in /app/data,
# but only when the mode is requested or was used on this volume, and `setup`
# only when CORESCOPE_RUN_AS_NONROOT is set to something other than unset,
# empty, 0 or false. On a volume that never used the mode, with the setting
# off, this file is never executed and the container starts exactly as before.
#
#   preflight            as root, before .env is sourced and config.json and
#                        theme.json are linked: refuse the start if the
#                        services could have written any of them
#   setup BASE_CONF      as root: create the run account, make the data dir
#                        usable but not ownable for it, hand the services'
#                        own files to it, set aside queue requests it did not
#                        write, write a supervisor config that runs only the
#                        two Go programs as that account, print its path
#   owners               print the owner and mode of the data dir and of
#                        every listed path ("UID:GID MODE NAME"), as a record
#                        for restore-owners
#   restore-owners [UID:GID]   as root, container stopped: apply a record read
#                        from stdin exactly; listed paths not in it (created
#                        while the mode was on) get UID:GID (default 0:0)
#   set-owner UID:GID    as root, container stopped: give the data dir and the
#                        listed paths to UID:GID (coarse rollback)
#   render-conf BASE_CONF USER   print BASE_CONF with user=USER added to the
#                        two Go programs (Mosquitto and Caddy untouched)
#   validate             check the settings, print UID:GID
#   paths                print the migration list ("f name" / "d name")
#
# The security boundary: root reads .env, config.json and theme.json in the
# data dir at every start, so the services must never be able to write or
# replace them. They therefore never own the data dir; they get group write
# on it (SQLite creates -wal/-shm/-journal files there) together with the
# sticky bit, so they cannot remove or rename entries they do not own. A file
# they create later under one of those names is refused by `preflight`.
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
# Root-owned record of the IDs the services last ran as. Its presence keeps
# `preflight` active after the setting is removed, until a rollback.
MARKER=.corescope-nonroot

# What the Go services create next to the database. Kept in sync with the Go
# constants by scripts/test-nonroot-helper.sh. Queue directories are flat.
DB_FILES="meshcore.db meshcore.db-wal meshcore.db-shm meshcore.db-journal ping_scores_history.db ping_scores_history.db-wal ping_scores_history.db-shm ping_scores_history.db-journal"
SIDE_FILES="client-rx-delete.salt client-rx-delete-audit.jsonl"
QUEUE_DIRS="prune-requests mbcap-snapshot channel-proposal-requests client-rx-deletes"
# What root uses from the data dir: sourced (.env) or linked into /app.
ROOT_FILES=".env config.json theme.json"

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
  valid_id "$uid" || die "CORESCOPE_UID='$uid' must be a non-zero numeric user ID, used by nothing else (e.g. 10001)"
  valid_id "$gid" || die "CORESCOPE_GID='$gid' must be a non-zero numeric group ID (e.g. 10001)"
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

# env_value KEY: the value root will see for KEY once .env is sourced — the
# inherited one, overridden by a plain KEY=VALUE line in .env (last wins).
# .env is only read as data here. A line that sets KEY in any other form
# stops the start, because sourcing could then give a value nobody checked.
env_value() {
  eval "val=\${$1-}"
  f=$DATA_DIR/.env
  if [ -f "$f" ] && [ ! -L "$f" ]; then
    set +e
    from_file=$(awk -v k="$1" -v q="'" '
      index($0, k) {
        s = $0
        sub(/^[ \t]+/, "", s)
        if (s ~ /^#/) next
        sub(/[ \t]+#.*$/, "", s)
        sub(/[ \t]+$/, "", s)
        sub(/^export[ \t]+/, "", s)
        if (substr(s, 1, length(k) + 1) == k "=") {
          v = substr(s, length(k) + 2)
          if (v ~ /^[A-Za-z0-9]*$/) { out = v; set = 1; next }
          if (v ~ /^"[A-Za-z0-9]*"$/ || v ~ ("^" q "[A-Za-z0-9]*" q "$")) {
            out = substr(v, 2, length(v) - 2); set = 1; next
          }
        }
        if ($0 ~ ("(^|[^A-Za-z0-9_])" k "([^A-Za-z0-9_]|$)")) bad = 1
      }
      END { if (bad) exit 3; if (set) print "=" out }
    ' "$f")
    rc=$?
    set -e
    [ "$rc" -eq 0 ] || die "$f sets $1 in a form that cannot be checked before it is sourced; write it as a plain line, $1=VALUE"
    [ -z "$from_file" ] || val=${from_file#=}
  fi
  printf '%s' "$val"
}

# read_marker: m_ids = the UID:GID the services last ran as, or empty.
read_marker() {
  m_ids=
  p=$DATA_DIR/$MARKER
  [ -e "$p" ] || [ -L "$p" ] || return 0
  if [ -L "$p" ] || [ ! -f "$p" ] || [ "$(stat -c '%u %h' "$p")" != "0 1" ] ||
    [ $((0$(stat -c %a "$p") & 022)) -ne 0 ]; then
    die "$p is not the root-owned file CoreScope writes; check how it got there, then remove it"
  fi
  m_ids=$(head -n 1 "$p")
  if ! valid_id "${m_ids%%:*}" || ! valid_id "${m_ids#*:}"; then
    die "$p does not hold UID:GID; check how it got there, then remove it"
  fi
}

# untrusted MESSAGE: stop because root would use a file the services could
# have written.
untrusted() {
  die "$1. Root uses this file at every start, so CORESCOPE_UID/GID must not be able to write or replace it. Check its content, give it to root or the host user without write access for that group, and start again (docs/deployment.md, \"Running the Go services without root\")"
}

# readonly_mount PATH: PATH is itself a read-only mount (e.g. a :ro bind
# mount of config.json). Nothing in the container can write it, and a mount
# point cannot be removed or renamed over.
readonly_mount() {
  awk -v p="$1" '$5 == p { ro = ($6 ~ /(^|,)ro(,|$)/) } END { exit !ro }' /proc/self/mountinfo
}

# check_trust: refuse when any UID:GID in $check_ids owns the data dir, or
# when one of ROOT_FILES could have been written by anyone but root or the
# data dir's owner. The data dir is writable for the services' group, so a
# file there must be owned by root or that owner (never by a service UID) and
# must not be writable for the services' group or for everyone.
check_trust() {
  dir_owner=$(stat -c %u "$DATA_DIR")
  for cid in $check_ids; do
    [ "$dir_owner" != "${cid%%:*}" ] ||
      die "CORESCOPE_UID=${cid%%:*} owns the data directory $DATA_DIR; the services must never own it. Use a dedicated ID, or give the directory back to root or the host user (docs/deployment.md, \"Running the Go services without root\")"
  done
  for name in $ROOT_FILES; do
    p=$DATA_DIR/$name
    [ ! -L "$p" ] || untrusted "$p is a symlink"
    [ -e "$p" ] || continue
    [ -f "$p" ] || untrusted "$p is not a regular file"
    # shellcheck disable=SC2046 # word splitting is intended
    set -- $(stat -c '%u %g %a %h' "$p")
    [ "$4" = 1 ] || untrusted "$p has $4 hard links"
    for cid in $check_ids; do
      [ "$1" != "${cid%%:*}" ] || untrusted "$p is owned by ${cid%%:*}, the services' user"
    done
    if readonly_mount "$p"; then continue; fi
    [ "$1" = 0 ] || [ "$1" = "$dir_owner" ] ||
      untrusted "$p is owned by $1, which is neither root nor the owner of $DATA_DIR ($dir_owner)"
    mode=$((0$3))
    [ $((mode & 002)) -eq 0 ] || untrusted "$p is world-writable"
    for cid in $check_ids; do
      [ $((mode & 020)) -eq 0 ] || [ "$2" != "${cid#*:}" ] ||
        untrusted "$p is writable for group ${cid#*:}, the services' group"
    done
  done
}

# preflight: see the header. Runs before .env is sourced, so the settings
# come from env_value.
preflight() {
  [ "$(id -u)" = 0 ] || die "preflight must run as root"
  [ -d "$DATA_DIR" ] || return 0
  read_marker
  CORESCOPE_RUN_AS_NONROOT=$(env_value CORESCOPE_RUN_AS_NONROOT)
  ids=
  case "$CORESCOPE_RUN_AS_NONROOT" in
    '' | 0 | false) ;;
    *)
      CORESCOPE_UID=$(env_value CORESCOPE_UID)
      CORESCOPE_GID=$(env_value CORESCOPE_GID)
      validate
      ids="$uid:$gid"
      ;;
  esac
  [ -n "$ids$m_ids" ] || return 0
  check_ids="$ids $m_ids"
  check_trust
  [ -n "$ids" ] || disarm_data_dir
}

# disarm_data_dir: the mode is off, but the data dir is still set up for it
# (rollback step 2 not done yet). Root then runs everything, and the kernel's
# fs.protected_regular would stop root from opening the services' files in a
# sticky directory that is group-writable. Take back group write and the
# sticky bit; restore-owners later restores the recorded mode exactly.
disarm_data_dir() {
  [ $((0$(stat -c %a "$DATA_DIR") & 01020)) -ne 0 ] || return 0
  chmod g-w,-t "$DATA_DIR" || die "could not change the mode of $DATA_DIR"
  log "the mode is off: removed group write and the sticky bit from $DATA_DIR so root can use the services' files; run restore-owners or set-owner (Rollback step 2) to finish the rollback"
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

# usable PATH: PATH may be chowned — not a symlink, not hard-linked, a regular
# file or a directory. Warns and fails otherwise.
usable() {
  if [ -L "$1" ]; then
    log "WARNING: $1 is a symlink; left unchanged"
    return 1
  fi
  [ -e "$1" ] || return 1
  if [ -f "$1" ]; then
    if [ "$(stat -c %h "$1")" -gt 1 ]; then
      log "WARNING: $1 is hard-linked; left unchanged"
      return 1
    fi
  elif [ ! -d "$1" ]; then
    log "WARNING: $1 is not a regular file or directory; left unchanged"
    return 1
  fi
}

# fix_owner PATH OWNER: chown PATH (itself only) when its owner differs.
fix_owner() {
  usable "$1" || return 0
  checked=$((checked + 1))
  [ "$(stat -c %u:%g "$1")" = "$2" ] && return 0
  chown -h "$2" "$1" || die "could not change the owner of $1 (read-only data volume?)"
  changed=$((changed + 1))
}

# migrate OWNER: the services' own files and queue directories.
migrate() {
  for f in $DB_FILES $SIDE_FILES; do fix_owner "$DATA_DIR/$f" "$1"; done
  for d in $QUEUE_DIRS; do fix_owner "$DATA_DIR/$d" "$1"; done
}

# each_queue_entry CMD [ARGS...]: run CMD ARGS... ENTRY for every direct
# entry of the queue directories. Globbed, so any file name is one entry; a
# queue directory that is a symlink is skipped.
each_queue_entry() {
  for d in $QUEUE_DIRS; do
    if [ ! -d "$DATA_DIR/$d" ] || [ -L "$DATA_DIR/$d" ]; then continue; fi
    for e in "$DATA_DIR/$d"/* "$DATA_DIR/$d"/.[!.]* "$DATA_DIR/$d"/..?*; do
      if [ -e "$e" ] || [ -L "$e" ]; then "$@" "$e"; fi
    done
  done
}

# owner_of OWNER PATH: fix_owner with the arguments in each_queue_entry order.
owner_of() { fix_owner "$2" "$1"; }

# secure_data_dir GID: group write for the services, through the group and
# never as owner, plus the sticky bit; the owner stays as it is.
secure_data_dir() {
  [ ! -L "$DATA_DIR" ] || die "$DATA_DIR is a symlink"
  set -- "$1" "$(stat -c %g "$DATA_DIR")" "$(stat -c %a "$DATA_DIR")"
  dir_changed=
  if [ "$2" != "$1" ]; then
    chgrp "$1" "$DATA_DIR" || die "could not change the group of $DATA_DIR (read-only data volume?)"
    dir_changed=1
  fi
  if [ $((0$3 & 01070)) -ne $((01070)) ]; then
    chmod g+rwx,+t "$DATA_DIR" || die "could not change the mode of $DATA_DIR"
    dir_changed=1
  fi
  [ -z "$dir_changed" ] || log "$DATA_DIR: group $1 with write access and the sticky bit; owner unchanged"
}

# hold_requests UID: a queued request the services did not write (a root
# `docker exec`, a restore, an earlier root run) is never handed to them. It
# is renamed to NAME.held, keeps its owner and is reported; running it takes
# an operator step.
hold_requests() {
  for d in $QUEUE_DIRS; do
    if [ ! -d "$DATA_DIR/$d" ] || [ -L "$DATA_DIR/$d" ]; then continue; fi
    for e in "$DATA_DIR/$d"/request-*.json "$DATA_DIR/$d"/cmd-*.json; do
      if [ ! -e "$e" ] && [ ! -L "$e" ]; then continue; fi
      [ "$(stat -c %u "$e")" != "$1" ] || continue
      held=$e.held
      n=1
      while [ -e "$held" ] || [ -L "$held" ]; do
        held=$e.held.$n
        n=$((n + 1))
      done
      mv "$e" "$held" || die "could not set aside $e"
      log "WARNING: $d/${e##*/} was not written by the services (owner $(stat -c %u:%g "$held")); renamed to ${held##*/} and NOT run. Repeat the command as $1 (docker exec -u), or rename the file back to run it"
    done
  done
}

# write_marker UID:GID: create or update the marker (never through a planted
# name: mktemp, then rename).
write_marker() {
  [ "$m_ids" != "$1" ] || return 0
  tmp=$(mktemp "$DATA_DIR/$MARKER.XXXXXX") || die "could not write $DATA_DIR/$MARKER"
  printf '%s\n' "$1" > "$tmp"
  chmod 644 "$tmp"
  mv -f "$tmp" "$DATA_DIR/$MARKER"
}

as_user() { su -s /bin/sh "$RUN_USER" -c "$1"; }

setup() {
  [ "$(id -u)" = 0 ] || die "setup must run as root"
  validate
  base=$1
  [ -d "$DATA_DIR" ] || die "$DATA_DIR does not exist"
  read_marker
  # Again after .env was sourced, with the IDs that will actually be used.
  check_ids="$uid:$gid $m_ids"
  check_trust
  ensure_account "$uid" "$gid"
  secure_data_dir "$gid"
  migrate "$uid:$gid"
  hold_requests "$uid"
  write_marker "$uid:$gid"
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

# rollback_check: a rollback hands the volume back to root, so it refuses
# while a file root uses could have been written by the services.
rollback_check() {
  [ "$(id -u)" = 0 ] || die "this command must run as root"
  [ -d "$DATA_DIR" ] || die "$DATA_DIR does not exist"
  read_marker
  check_ids=$m_ids
  [ -z "$m_ids" ] || check_trust
}

valid_owner() {
  case "$1" in *:*) ;; *) return 1 ;; esac
  for id in "${1%%:*}" "${1#*:}"; do
    [ "$id" = 0 ] || valid_id "$id" || return 1
  done
}

owners() {
  [ -d "$DATA_DIR" ] || die "$DATA_DIR does not exist"
  echo "$(stat -c '%u:%g %a' "$DATA_DIR") ."
  for f in $DB_FILES $SIDE_FILES $QUEUE_DIRS; do
    if [ -e "$DATA_DIR/$f" ] && [ ! -L "$DATA_DIR/$f" ]; then
      echo "$(stat -c '%u:%g %a' "$DATA_DIR/$f") $f"
    fi
  done
  each_queue_entry record_entry
}

record_entry() {
  [ ! -L "$1" ] || return 0
  case "${1##*/}" in
    *[[:space:]]*) log "WARNING: not recording $1 (white space in its name)" ;;
    *) echo "$(stat -c '%u:%g %a' "$1") ${1#"$DATA_DIR"/}" ;;
  esac
}

# unrecorded_to OWNER PATH: give PATH to OWNER unless the record names it.
unrecorded_to() {
  printf '%s\n' "$record" | awk -v n="${2#"$DATA_DIR"/}" '$3 == n { found = 1 } END { exit !found }' ||
    fix_owner "$2" "$1"
}

# recordable NAME: NAME is the data dir, a listed path or a direct entry of a
# listed queue directory.
recordable() {
  [ "$1" = . ] && return 0
  for f in $DB_FILES $SIDE_FILES $QUEUE_DIRS; do [ "$1" != "$f" ] || return 0; done
  for d in $QUEUE_DIRS; do
    case "$1" in
      "$d"/*/* | "$d"/. | "$d"/..) ;;
      "$d"/?*) return 0 ;;
    esac
  done
  return 1
}

restore_owners() {
  default=${1:-0:0}
  valid_owner "$default" || die "usage: restore-owners [UID:GID] < record"
  rollback_check
  record=$(cat)
  n=0
  has_dir=
  while read -r own mode name rest; do
    n=$((n + 1))
    [ -n "$own$mode$name" ] || continue
    case "$mode" in *[!0-7]* | ?????*) mode= ;; ???*) ;; *) mode= ;; esac
    if [ -n "$rest" ] || [ -z "$mode" ] || ! valid_owner "$own" || ! recordable "$name"; then
      die "record line $n is not 'UID:GID MODE NAME' for the data dir or a listed path"
    fi
    [ "$name" != . ] || has_dir=1
  done << EOF
$record
EOF
  [ -n "$has_dir" ] || die "the record has no line for the data dir ('UID:GID MODE .'); use the output of 'nonroot.sh owners'"

  # Listed paths created while the mode was on go to the default owner ...
  for f in $DB_FILES $SIDE_FILES $QUEUE_DIRS; do unrecorded_to "$default" "$DATA_DIR/$f"; done
  each_queue_entry unrecorded_to "$default"
  # ... and every recorded one gets its recorded owner and mode back.
  while read -r own mode name; do
    [ -n "$name" ] || continue
    p=$DATA_DIR/$name
    [ "$name" != . ] || p=$DATA_DIR
    if [ ! -e "$p" ] && [ ! -L "$p" ]; then
      log "$name is gone; nothing to restore"
      continue
    fi
    case "$name" in
      */*) if [ ! -d "${p%/*}" ] || [ -L "${p%/*}" ]; then continue; fi ;;
    esac
    [ "$name" = . ] || usable "$p" || continue
    fix_owner "$p" "$own"
    [ "$(stat -c %a "$p")" = "$mode" ] || chmod "$mode" "$p" || die "could not restore the mode of $p"
  done << EOF
$record
EOF
  rm -f "$DATA_DIR/$MARKER"
  log "ownership: $changed of $checked path(s) in $DATA_DIR restored; modes as recorded"
}

set_owner() {
  valid_owner "$1" || die "usage: set-owner UID:GID"
  rollback_check
  fix_owner "$DATA_DIR" "$1"
  chmod g-w,-t "$DATA_DIR" || die "could not change the mode of $DATA_DIR"
  migrate "$1"
  each_queue_entry owner_of "$1"
  rm -f "$DATA_DIR/$MARKER"
  log "ownership: $changed of $checked path(s) in $DATA_DIR changed to $1"
}

cmd=${1-}
[ $# -eq 0 ] || shift
case "$cmd" in
  preflight) [ $# -eq 0 ] || die "usage: $0 preflight"; preflight ;;
  setup) [ $# -eq 1 ] || die "usage: $0 setup BASE_CONF"; setup "$1" ;;
  owners) [ $# -eq 0 ] || die "usage: $0 owners"; owners ;;
  restore-owners) [ $# -le 1 ] || die "usage: $0 restore-owners [UID:GID] < record"; restore_owners "${1-}" ;;
  set-owner) [ $# -eq 1 ] || die "usage: $0 set-owner UID:GID"; set_owner "$1" ;;
  render-conf) [ $# -eq 2 ] || die "usage: $0 render-conf BASE_CONF USER"; render_conf "$1" "$2" ;;
  validate) validate; echo "$uid:$gid" ;;
  paths) paths ;;
  *) die "usage: $0 preflight|setup|owners|restore-owners|set-owner|render-conf|validate|paths" ;;
esac
