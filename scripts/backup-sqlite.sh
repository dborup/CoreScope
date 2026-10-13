#!/bin/sh
# Offline backup of the two CoreScope SQLite databases, including any WALs.
# Usage: sh scripts/backup-sqlite.sh CONTAINER BACKUP_DIRECTORY
# Requires Docker and ordinary host shell tools; never invokes SQLite or pulls
# an image. The container's /app/data must not have another active writer.
# Runs for the same container are serialised by a host-local lock: see
# CORESCOPE_BACKUP_LOCK_DIR and CORESCOPE_BACKUP_LOCK_WAIT in docs/deployment.md.
set -eu
# Even under `sh -m`: with job control every child leads its own process
# group, and setsid(1) would then fork and return early (see isolated below).
set +m
umask 077

if [ "$#" -ne 2 ]; then
  echo "usage: $0 CONTAINER BACKUP_DIRECTORY" >&2
  exit 2
fi
container=$1
case "$container" in
  ''|-*|*[!a-zA-Z0-9_.-]*) echo 'invalid container name or ID' >&2; exit 2 ;;
esac
# Not $TMPDIR, which differs per user on macOS and is dropped by sudo: every
# invocation on the host must find the same lock.
lock_root=${CORESCOPE_BACKUP_LOCK_DIR-/tmp}
case "$lock_root" in /*) ;; *) echo 'CORESCOPE_BACKUP_LOCK_DIR must be an absolute path' >&2; exit 2 ;; esac
lock_wait=${CORESCOPE_BACKUP_LOCK_WAIT-600}
case "$lock_wait" in
  ''|*[!0-9]*) echo 'CORESCOPE_BACKUP_LOCK_WAIT must be a whole number of seconds' >&2; exit 2 ;;
esac

# The lock is keyed by the full container ID, which a name, the ID and any
# unique ID prefix all resolve to. Failures are fatal before any lock or stop.
id=$(docker inspect --format '{{.Id}}' "$container")
case "$id" in *[!0-9a-f]*) id= ;; esac
if [ "${#id}" -ne 64 ]; then echo 'unknown container ID' >&2; exit 1; fi
lock=$lock_root/corescope-backup-$id.lock
if command -v flock >/dev/null 2>&1; then lock_method=flock; else lock_method=mkdir; fi
lock_held=0
stage=
backup_root=
restart_needed=0

# Docker's CLI re-enables SIGINT/SIGTERM even when they are inherited as
# ignored, so a repeated Ctrl-C or process-group signal would kill it. Where
# setsid(1) exists, run it in its own session, out of reach of those signals.
# Without job control (set +m above) children are never process-group leaders,
# so setsid execs in place and the shell still waits for the command.
isolated() {
  if command -v setsid >/dev/null 2>&1; then setsid "$@"; else "$@"; fi
}

# A zero exit from `docker start` alone is not proof: it is a no-op on a
# container that is still running (or still stopping).
start_and_verify() {
  isolated docker start "$container" >/dev/null || return
  state=$(isolated docker inspect --format '{{.State.Running}}' "$container") || return
  if [ "$state" != true ]; then
    echo "ERROR: $container is not running after start" >&2 || :
    return 1
  fi
}

# A signal can kill the docker CLI while the daemon is still stopping the
# container. Wait for any stop in progress (stop is idempotent) and confirm
# that the container has exited, so the next start really starts it. Up to
# three attempts because, without setsid, a repeated signal can kill the CLI.
stop_confirmed() {
  tries=0
  while [ "$tries" -lt 3 ]; do
    tries=$((tries + 1))
    if isolated docker stop "$container" >/dev/null &&
      state=$(isolated docker inspect --format '{{.State.Running}}' "$container") &&
      [ "$state" = false ]; then
      return 0
    fi
  done
  echo "ERROR: could not confirm that $container stopped" >&2 || :
  return 1
}

# flock(1) locks an open descriptor, which the kernel releases when the last
# process holding it exits, so it never goes stale. Without flock (macOS) an
# atomic mkdir is the lock and records its owner's PID.
try_lock() {
  if [ "$lock_method" = flock ]; then
    flock -n 9 || return
    lock_held=1
    return 0
  fi
  # Signals wait until the lock directory and its owner file are consistent,
  # so neither an ownerless lock nor a half-reclaimed one is left behind.
  pending=
  trap 'pending="HUP 129"' HUP
  trap 'pending="INT 130"' INT
  trap 'pending="TERM 143"' TERM
  acquired=1
  lock_error=
  if mkdir -- "$lock" 2>/dev/null || { reclaim_stale_lock && mkdir -- "$lock" 2>/dev/null; }; then
    lock_held=1
    if printf '%s\n' "$$" >"$lock/pid"; then acquired=0; else lock_error="cannot record the owner of $lock"; fi
  fi
  set_signal_traps
  if [ -n "$pending" ]; then interrupted $pending; fi
  if [ -n "$lock_error" ]; then echo "ERROR: $lock_error" >&2; exit 1; fi
  return "$acquired"
}

# A lock whose owner no longer exists is stale (SIGKILL, a crash) and is
# removed; a live or unknown owner never is. Only one invocation reclaims at a
# time, re-reading the owner under that guard.
reclaim_stale_lock() {
  mkdir -- "$lock.reclaim" 2>/dev/null || return
  owner=
  if [ -f "$lock/pid" ] && [ ! -L "$lock/pid" ]; then IFS= read -r owner <"$lock/pid" || owner=; fi
  reclaimed=1
  if owner_gone "$owner" && rm -f -- "$lock/pid" && rmdir -- "$lock"; then
    reclaimed=0
    echo "Removed a stale backup lock left by PID $owner ($lock)" >&2 || :
  fi
  rmdir -- "$lock.reclaim" || :
  return "$reclaimed"
}

# kill -0 also fails for a live process of another user, so only ps(1) saying
# "no such process" (exit 1) counts as gone, and only from a ps that finds us.
owner_gone() {
  case "$1" in ''|*[!0-9]*) return 1 ;; esac
  if kill -0 "$1" 2>/dev/null || ! ps -p "$$" >/dev/null 2>&1; then return 1; fi
  gone=0
  ps -p "$1" >/dev/null 2>&1 || gone=$?
  [ "$gone" -eq 1 ]
}

# Wait up to $lock_wait seconds for another invocation, then fail with
# EX_TEMPFAIL (75) without having read or changed the container's state.
acquire_lock() {
  if [ "$lock_method" = flock ]; then kind=-f; else kind=-d; fi
  if [ -L "$lock" ] || { [ -e "$lock" ] && [ ! "$kind" "$lock" ]; }; then
    echo "ERROR: $lock is not a $lock_method lock; remove it once no backup of $container is running" >&2
    exit 1
  fi
  if [ "$lock_method" = flock ] && ! command exec 9>>"$lock"; then
    echo "ERROR: cannot open the lock file $lock" >&2
    exit 1
  fi
  waited=0
  until try_lock; do
    if [ "$waited" -ge "$lock_wait" ]; then
      echo "ERROR: another backup of $container still holds $lock after ${lock_wait}s; nothing was stopped" >&2
      exit 75
    fi
    if [ "$waited" -eq 0 ]; then
      echo "Waiting up to ${lock_wait}s for another backup of $container to finish ($lock)" >&2
    fi
    sleep 1
    waited=$((waited + 1))
  done
}

# Only after every stop/start of this invocation has completed.
release_lock() {
  [ "$lock_held" -eq 1 ] || return 0
  lock_held=0
  if [ "$lock_method" = flock ]; then
    exec 9>&-
  else
    rm -f -- "$lock/pid" && rmdir -- "$lock"
  fi
}

finish() {
  status=$?
  # The shell itself must survive a repeated signal during recovery.
  trap '' HUP INT TERM
  trap - 0
  if [ "$restart_needed" -eq 1 ]; then
    stopped=0
    stop_confirmed && stopped=1
    # Start even after an unconfirmed stop, which may have stopped it, but a
    # running state is then no proof of a restart.
    if start_and_verify; then
      if [ "$stopped" -eq 0 ]; then
        echo "ERROR: restart not verified; check $container manually" >&2 || :
        if [ "$status" -eq 0 ]; then status=1; fi
      fi
    else
      restart_status=$?
      echo "ERROR: restart failed (exit $restart_status); check $container manually" >&2 || :
      if [ "$status" -eq 0 ]; then status=$restart_status; fi
    fi
  fi
  # Only this invocation's fresh, private staging directory is removed.
  case "$stage" in
    "$backup_root"/.corescope-backup-*)
      if ! rm -rf "$stage"; then
        echo "ERROR: could not remove private staging directory $stage" >&2 || :
        if [ "$status" -eq 0 ]; then status=1; fi
      fi ;;
  esac
  if ! release_lock; then
    echo "ERROR: could not remove the backup lock $lock" >&2 || :
    if [ "$status" -eq 0 ]; then status=1; fi
  fi
  exit "$status"
}
interrupted() {
  # Shield from here on: a further signal must not preempt the EXIT trap,
  # whose own `trap ''` only runs after it has started.
  trap '' HUP INT TERM
  echo "ERROR: interrupted by SIG$1" >&2 || :
  exit "$2"
}
set_signal_traps() {
  trap 'interrupted HUP 129' HUP
  trap 'interrupted INT 130' INT
  trap 'interrupted TERM 143' TERM
}
trap finish 0
set_signal_traps

mkdir -p -- "$lock_root"
acquire_lock

# Preserve an intentionally stopped container's state. Inspection failures are
# fatal before any stop or data copy; no guessing from Docker error text.
running=$(docker inspect --format '{{.State.Running}}' "$container")
case "$running" in true|false) ;; *) echo 'unknown container running state' >&2; exit 1 ;; esac
# A leading "-" is neither an option nor `cd -`; CDPATH must not redirect cd.
case "$2" in -*) target=./$2 ;; *) target=$2 ;; esac
mkdir -p -- "$target"
backup_root=$(CDPATH='' cd -- "$target" && pwd -P)
stage=$(mktemp -d "$backup_root/.corescope-backup-$(date -u +%Y%m%dT%H%M%SZ).XXXXXX")
chmod 700 "$stage"

if [ "$running" = true ]; then
  # Even a failed stop may have stopped the container before its API call
  # failed. Attempt to restore availability in that case, preserving the error.
  restart_needed=1
  docker stop "$container" >/dev/null
fi
mkdir "$stage/raw" "$stage/snapshot"
# Copy the directory in one operation: absent WALs are naturally absent;
# failing to copy an existing WAL (ENOSPC, permissions, etc.) is a real error.
# The private raw copy can include config/secrets, but only the whitelist below
# is published; enough temporary disk for the complete /app/data is required.
docker cp "$container:/app/data/." "$stage/raw"
for db in meshcore ping_scores_history; do
  file="$stage/raw/$db.db"
  if [ ! -f "$file" ] || [ -L "$file" ] || [ ! -r "$file" ]; then
    echo "ERROR: missing or unreadable regular database $db.db" >&2
    exit 1
  fi
  mv "$file" "$stage/snapshot/$db.db"
  chmod 600 "$stage/snapshot/$db.db"
  file="$stage/raw/$db.db-wal"
  if [ -e "$file" ] || [ -L "$file" ]; then
    if [ ! -f "$file" ] || [ -L "$file" ] || [ ! -r "$file" ]; then
      echo "ERROR: unreadable regular WAL $db.db-wal" >&2
      exit 1
    fi
    mv "$file" "$stage/snapshot/$db.db-wal"
    chmod 600 "$stage/snapshot/$db.db-wal"
  fi
done

if [ "$restart_needed" -eq 1 ]; then
  if start_and_verify; then
    restart_needed=0
  else
    restart_status=$?
    # A returned failure is reported once; a signal during start instead
    # leaves restart_needed set so the EXIT trap attempts recovery.
    restart_needed=0
    echo "ERROR: restart failed (exit $restart_status); backup not published; check $container manually" >&2
    exit "$restart_status"
  fi
fi
# Fresh output directory for every invocation, even several runs in one second.
# Publish only after both databases/WALs were copied and restart succeeded.
name=${stage##*/}
destination="$backup_root/${name#.}"
if [ -e "$destination" ] || [ -L "$destination" ]; then
  echo "ERROR: refusing to overwrite $destination" >&2
  exit 1
fi
mv "$stage/snapshot" "$destination"
printf '%s\n' "$destination"
