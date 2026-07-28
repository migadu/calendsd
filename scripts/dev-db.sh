#!/usr/bin/env bash
# Manages the shared development Postgres both servers (sabre reference + Go)
# run against. Uses a unix socket under .dev/ so nothing touches system state.
#
#   scripts/dev-db.sh start   # initdb (first run), start, create + load schema
#   scripts/dev-db.sh stop
#   scripts/dev-db.sh reset   # drop and reload the schema
set -euo pipefail

cd "$(dirname "$0")/.."
: "${PGDATA:=$PWD/.dev/pgdata}"
: "${PGHOST:=$PWD/.dev/pg}"
: "${PGDATABASE:=calserver}"
export PGDATA PGHOST PGDATABASE

load_schema() {
    for f in dav/examples/sql/pgsql.*.sql; do
        psql -q -v ON_ERROR_STOP=1 -d "$PGDATABASE" -f "$f"
    done
    # calserver extension tables (notifications, publications) — additive,
    # new tables only, so a live sabre on the same DB is unaffected.
    for f in scripts/schema/pgsql.*.sql; do
        psql -q -v ON_ERROR_STOP=1 -d "$PGDATABASE" -f "$f"
    done
    # examples/sql seeds principals/users with an 'admin' account
    # (digesta1 for realm "SabreDAV", password "admin"), but no calendar;
    # give admin an empty default calendar the same way sabre's PDO
    # backend would.
    psql -q -v ON_ERROR_STOP=1 -d "$PGDATABASE" <<'SQL'
INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT,VTODO');
INSERT INTO calendarinstances (calendarid, principaluri, access, displayname, uri, description, calendarorder, calendarcolor, timezone, transparent)
SELECT currval('calendars_id_seq'), 'principals/admin', 1, 'default calendar', 'default', '', 0, NULL, NULL, 0;
SQL
    # Second principal so sharing, proxy delegation, and local iTIP have a
    # target (mirrors what storage.AdminWrites.CreateUser inserts: the users
    # row plus the principal AND its calendar-proxy children — without the
    # proxy rows delegation to bob silently breaks).
    # digesta1 = md5("bob:SabreDAV:bob"); password "bob".
    psql -q -v ON_ERROR_STOP=1 -d "$PGDATABASE" <<'SQL'
INSERT INTO users (username, digesta1) VALUES
('bob', '40d17625c1ef3974fc1138a86b8248fc');
INSERT INTO principals (uri, email, displayname) VALUES
('principals/bob', 'bob@example.org', 'Bob'),
('principals/bob/calendar-proxy-read', NULL, NULL),
('principals/bob/calendar-proxy-write', NULL, NULL);
INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT,VTODO');
INSERT INTO calendarinstances (calendarid, principaluri, access, displayname, uri, description, calendarorder, calendarcolor, timezone, transparent)
SELECT currval('calendars_id_seq'), 'principals/bob', 1, 'default calendar', 'default', '', 0, NULL, NULL, 0;
SQL
}

case "${1:-}" in
start)
    mkdir -p "$PGHOST"
    if [ ! -f "$PGDATA/PG_VERSION" ]; then
        initdb -U "$USER" -D "$PGDATA" -E UTF8 --no-locale >/dev/null
    fi
    if ! pg_ctl status >/dev/null 2>&1; then
        pg_ctl -l "$PGDATA/log" -o "-k '$PGHOST' -c listen_addresses=''" start
    fi
    if ! psql -lqt | cut -d'|' -f1 | grep -qw "$PGDATABASE"; then
        createdb "$PGDATABASE"
        load_schema
        echo "created and loaded $PGDATABASE"
    fi
    ;;
stop)
    pg_ctl stop -m fast
    ;;
reset)
    dropdb --if-exists "$PGDATABASE"
    createdb "$PGDATABASE"
    load_schema
    echo "reloaded $PGDATABASE"
    ;;
*)
    echo "usage: $0 start|stop|reset" >&2
    exit 1
    ;;
esac
