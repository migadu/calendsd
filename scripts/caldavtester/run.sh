#!/usr/bin/env bash
# Run Apple's CalDAVTester conformance suite against a local calserver.
#
#   direnv exec . scripts/caldavtester/run.sh          # run the curated list
#   direnv exec . scripts/caldavtester/run.sh CalDAV/put.xml   # one file
#
# CalDAVTester is Python 2 only and Python 2 is EOL, so this builds a py2.7
# interpreter from the nixos-26.05 channel (allowing the insecure package) and
# checks out CalDAVTester + pycalendar under .dev/. It seeds user01/user02 into
# the shared dev database, starts a calserver bound to 127.0.0.1:$PORT, and
# replays each test file. Because the suite assumes Calendar-Server-style
# auto-provisioning (a default calendar/addressbook that returns after
# DELETEALL), the accounts are re-seeded before every file.
#
# Prereqs: the nix dev shell (direnv) for go/psql; scripts/dev-db.sh Postgres.
set -euo pipefail

cd "$(dirname "$0")/../.."
ROOT="$PWD"
HERE="$ROOT/scripts/caldavtester"
WORK="$ROOT/.dev/caldavtester"
PORT="${PORT:-8008}"
CTESTER_REF="${CTESTER_REF:-bed21e5924275552c1561febc8203a9f194cf737}"

: "${PGHOST:=$ROOT/.dev/pg}"
: "${PGDATABASE:=calserver}"
export PGHOST PGDATABASE
DSN="postgres://$USER@/$PGDATABASE?host=$PGHOST"

mkdir -p "$WORK"

echo "==> building Python 2.7 (nixos-26.05, insecure-allowed)"
NIXPKGS_ALLOW_INSECURE=1 nix build --impure \
  'github:NixOS/nixpkgs/nixos-26.05#python2' -o "$WORK/py2" >/dev/null
PY2="$(readlink -f "$WORK/py2")/bin/python2.7"
"$PY2" --version

if [ ! -d "$WORK/caldavtester/.git" ]; then
    echo "==> cloning CalDAVTester @ ${CTESTER_REF:0:12}"
    git clone https://github.com/apple/ccs-caldavtester "$WORK/caldavtester"
    git -C "$WORK/caldavtester" checkout -q "$CTESTER_REF"
fi
if [ ! -d "$WORK/pycalendar/.git" ]; then
    echo "==> cloning pycalendar"
    git clone --depth 1 https://github.com/apple/ccs-pycalendar.git "$WORK/pycalendar"
fi

echo "==> tailoring serverinfo.xml for calserver"
"$PY2" "$HERE/serverinfo-overrides.py" \
    "$WORK/caldavtester/scripts/server/serverinfo.xml" 127.0.0.1 "$PORT"

echo "==> ensuring dev database is up + schema loaded"
"$ROOT/scripts/dev-db.sh" start >/dev/null

seed() { psql -q -v ON_ERROR_STOP=1 -d "$PGDATABASE" -f "$HERE/seed.sql"; }
echo "==> seeding test accounts"
seed

echo "==> building calserver"
go build -o "$WORK/calserver" ./cmd/calserver

"$WORK/calserver" -addr "127.0.0.1:$PORT" -db "$DSN" -realm SabreDAV \
    -browser -auto-schedule-prefixes resource,location -principals-readable -auto-provision \
    >"$WORK/server.log" 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null || true' EXIT
sleep 1

files=("$@")
if [ ${#files[@]} -eq 0 ]; then
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        case "$line" in \#*) continue ;; esac
        files+=("$line")
    done < "$HERE/tests.txt"
fi

export PYTHONPATH="$WORK/pycalendar/src:$WORK/caldavtester"
cd "$WORK/caldavtester"

printf "\n%-38s %6s %6s %6s %6s\n" "FILE" "ok" "fail" "ignd" "err"
to=0; tf=0; ti=0; te=0
for f in "${files[@]}"; do
    seed  # restore default collections a prior file's DELETEALL removed
    out="$("$PY2" testcaldav.py "$f" 2>/dev/null || true)"
    s="$(printf '%s' "$out" | grep -E '\(ok=' | tail -1)"
    ok=$(printf '%s' "$s" | sed -nE 's/.*ok=([0-9]+).*/\1/p'); ok=${ok:-0}
    fa=$(printf '%s' "$s" | sed -nE 's/.*failed=([0-9]+).*/\1/p'); fa=${fa:-0}
    ig=$(printf '%s' "$s" | sed -nE 's/.*ignored=([0-9]+).*/\1/p'); ig=${ig:-0}
    er=$(printf '%s' "$s" | sed -nE 's/.*errors=([0-9]+).*/\1/p'); er=${er:-0}
    printf "%-38s %6s %6s %6s %6s\n" "$f" "$ok" "$fa" "$ig" "$er"
    to=$((to+ok)); tf=$((tf+fa)); ti=$((ti+ig)); te=$((te+er))
done
echo   "-------------------------------------------------------------"
printf "%-38s %6s %6s %6s %6s\n" "TOTAL" "$to" "$tf" "$ti" "$te"
