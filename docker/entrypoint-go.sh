#!/bin/sh

# Opt-in (#339): run corescope-server and corescope-ingestor as
# CORESCOPE_UID:CORESCOPE_GID. Unset, empty, 0 or false keeps the historical
# start, in which every program runs as root.
nonroot_on() {
  case "${CORESCOPE_RUN_AS_NONROOT-}" in '' | 0 | false) return 1 ;; esac
}

# In that mode the services can write in /app/data. Before root trusts a file
# there (.env is sourced, config.json and theme.json are linked), the helper
# refuses the start if they could have written it. It runs only when the mode
# is requested here or in .env, or was used on this volume (its marker); a
# volume that never used it starts exactly as before.
if nonroot_on || [ -e /app/data/.corescope-nonroot ] || [ -L /app/data/.corescope-nonroot ] ||
  { [ -f /app/data/.env ] && [ ! -L /app/data/.env ] && grep -q CORESCOPE_RUN_AS_NONROOT /app/data/.env; }; then
  /usr/local/lib/corescope/nonroot.sh preflight || exit 1
fi

# Config lives in the data directory (bind-mounted from host)
# The Go server already searches /app/data/config.json via LoadConfig
# but the ingestor expects a direct path — symlink for compatibility
if [ -f /app/data/config.json ]; then
  ln -sf /app/data/config.json /app/config.json
elif [ ! -f /app/config.json ]; then
  echo "[entrypoint] No config.json found in /app/data/ — using built-in defaults"
fi

# theme.json: check data/ volume (admin-editable on host)
if [ -f /app/data/theme.json ]; then
  ln -sf /app/data/theme.json /app/theme.json
fi

# Source .env from data volume if present (works with any launch method)
if [ -f /app/data/.env ]; then
  set -a
  . /app/data/.env
  set +a
fi

SUPERVISORD_CONF="/etc/supervisor/conf.d/supervisord.conf"
if [ "${DISABLE_MOSQUITTO:-false}" = "true" ] && [ "${DISABLE_CADDY:-false}" = "true" ]; then
  echo "[config] internal MQTT broker disabled (DISABLE_MOSQUITTO=true)"
  echo "[config] Caddy reverse proxy disabled (DISABLE_CADDY=true)"
  SUPERVISORD_CONF="/etc/supervisor/conf.d/supervisord-no-mosquitto-no-caddy.conf"
elif [ "${DISABLE_MOSQUITTO:-false}" = "true" ]; then
  echo "[config] internal MQTT broker disabled (DISABLE_MOSQUITTO=true)"
  SUPERVISORD_CONF="/etc/supervisor/conf.d/supervisord-no-mosquitto.conf"
elif [ "${DISABLE_CADDY:-false}" = "true" ]; then
  echo "[config] Caddy reverse proxy disabled (DISABLE_CADDY=true)"
  SUPERVISORD_CONF="/etc/supervisor/conf.d/supervisord-no-caddy.conf"
fi

if nonroot_on; then
  SUPERVISORD_CONF=$(/usr/local/lib/corescope/nonroot.sh setup "$SUPERVISORD_CONF") || exit 1
fi

exec /usr/bin/supervisord -c "$SUPERVISORD_CONF"
