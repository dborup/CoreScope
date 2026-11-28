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

# The bundled mosquitto is opt-in (#441): it runs only when DISABLE_MOSQUITTO
# is a recognised false value. Trimmed and case-insensitive, true/1/yes/on
# disable it and false/0/no/off enable it; unset, empty and anything else
# take the default, true. manage.sh (parse_disable_mosquitto) and the
# ingestor (ResolveBrokerMode) read it with the same table; see
# test-fixtures/disable-mosquitto-cases.json.
DISABLE_MOSQUITTO_NORM=$(printf '%s' "${DISABLE_MOSQUITTO-}" |
  sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' | tr '[:upper:]' '[:lower:]')
case "$DISABLE_MOSQUITTO_NORM" in
  true | 1 | yes | on | '') MOSQUITTO_DISABLED=true ;;
  false | 0 | no | off) MOSQUITTO_DISABLED=false ;;
  *)
    echo "[config] DISABLE_MOSQUITTO='${DISABLE_MOSQUITTO}' is not a recognised value (true/1/yes/on or false/0/no/off); using the default, true"
    MOSQUITTO_DISABLED=true
    ;;
esac
if [ -n "${DISABLE_MOSQUITTO-}" ]; then
  MOSQUITTO_REASON="DISABLE_MOSQUITTO=${DISABLE_MOSQUITTO}"
else
  MOSQUITTO_REASON="DISABLE_MOSQUITTO unset; the bundled broker is opt-in, set DISABLE_MOSQUITTO=false to run it"
fi

SUPERVISORD_CONF="/etc/supervisor/conf.d/supervisord.conf"
if [ "$MOSQUITTO_DISABLED" = "true" ] && [ "${DISABLE_CADDY:-false}" = "true" ]; then
  echo "[config] internal MQTT broker disabled (${MOSQUITTO_REASON})"
  echo "[config] Caddy reverse proxy disabled (DISABLE_CADDY=true)"
  SUPERVISORD_CONF="/etc/supervisor/conf.d/supervisord-no-mosquitto-no-caddy.conf"
elif [ "$MOSQUITTO_DISABLED" = "true" ]; then
  echo "[config] internal MQTT broker disabled (${MOSQUITTO_REASON})"
  SUPERVISORD_CONF="/etc/supervisor/conf.d/supervisord-no-mosquitto.conf"
elif [ "${DISABLE_CADDY:-false}" = "true" ]; then
  echo "[config] Caddy reverse proxy disabled (DISABLE_CADDY=true)"
  SUPERVISORD_CONF="/etc/supervisor/conf.d/supervisord-no-caddy.conf"
fi

if nonroot_on; then
  SUPERVISORD_CONF=$(/usr/local/lib/corescope/nonroot.sh setup "$SUPERVISORD_CONF") || exit 1
fi

exec /usr/bin/supervisord -c "$SUPERVISORD_CONF"
