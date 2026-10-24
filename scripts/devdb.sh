#!/usr/bin/env bash
# Throwaway Postgres cluster for development and tests. Never touches your real databases.
# Usage: scripts/devdb.sh start|stop|restart|reset
set -euo pipefail
DIR="${DEVDB_DIR:-$(cd "$(dirname "$0")/.." && pwd)/.devdb}"
PORT="${DEVDB_PORT:-55432}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

start() {
  if [ ! -d "$DIR/data" ]; then
    mkdir -p "$DIR"
    initdb -D "$DIR/data" -U postgres --auth=trust -E UTF8 >/dev/null
    pg_ctl -D "$DIR/data" -o "-p $PORT -k $DIR -c listen_addresses=127.0.0.1" -l "$DIR/log" -w start >/dev/null
    psql -h 127.0.0.1 -p "$PORT" -U postgres -q -v ON_ERROR_STOP=1 -f "$ROOT/fixture/fixture.sql" >/dev/null
    echo "devdb created and seeded on 127.0.0.1:$PORT"
  else
    pg_ctl -D "$DIR/data" -o "-p $PORT -k $DIR -c listen_addresses=127.0.0.1" -l "$DIR/log" -w start >/dev/null
    echo "devdb started on 127.0.0.1:$PORT"
  fi
}
stop() { pg_ctl -D "$DIR/data" -m fast -w stop >/dev/null && echo "devdb stopped"; }

case "${1:-start}" in
  start) start ;;
  stop) stop ;;
  restart) pg_ctl -D "$DIR/data" -m fast -w restart >/dev/null && echo "devdb restarted" ;;
  reset) stop 2>/dev/null || true; rm -rf "$DIR"; start ;;
  *) echo "usage: $0 start|stop|restart|reset"; exit 1 ;;
esac
