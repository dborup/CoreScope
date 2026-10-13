#!/bin/sh
# Offline backup of the two CoreScope SQLite databases, including any WALs.
# Usage: sh scripts/backup-sqlite.sh CONTAINER BACKUP_DIRECTORY
# Requires Docker and ordinary host shell tools; never invokes SQLite or pulls
# an image. The container's /app/data must not have another active writer.
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
  exit "$status"
}
interrupted() {
  # Shield from here on: a further signal must not preempt the EXIT trap,
  # whose own `trap ''` only runs after it has started.
  trap '' HUP INT TERM
  echo "ERROR: interrupted by SIG$1" >&2 || :
  exit "$2"
}
trap finish 0
trap 'interrupted HUP 129' HUP
trap 'interrupted INT 130' INT
trap 'interrupted TERM 143' TERM

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
