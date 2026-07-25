#!/usr/bin/env bash
# Run the ENTIRE CalDAVTester suite (all ~193 files) against calserver and print
# a categorized breakdown. Companion to run.sh (which runs a curated subset).
#
#   direnv exec . scripts/caldavtester/run-full.sh
#
# Uses the FULL upstream feature list so every file executes (raw spread), and
# starts a FRESH server per file so cumulative resource use can't cascade the
# back half into false errors. A file that kills its fresh server is reported as
# CRASH (a real bug), distinct from ERROR (its <start> provisioning needs
# Calendar-Server infrastructure — groups, resources, proxies — that calserver
# does not provide).
#
# Categories: PASS (all ran green) · PARTIAL (mixed) · FAIL (ran, all failed) ·
# ERROR (start failed) · SKIP (nothing ran) · CRASH (server died).
set -uo pipefail

cd "$(dirname "$0")/../.."
ROOT="$PWD"
HERE="$ROOT/scripts/caldavtester"
WORK="$ROOT/.dev/caldavtester"
PORT="${PORT:-8008}"
CTESTER_REF="${CTESTER_REF:-bed21e5924275552c1561febc8203a9f194cf737}"
: "${PGHOST:=$ROOT/.dev/pg}"; : "${PGDATABASE:=calserver}"; export PGHOST PGDATABASE
DSN="postgres://$USER@/$PGDATABASE?host=$PGHOST"

mkdir -p "$WORK"
echo "==> building Python 2.7 (nixos-26.05, insecure-allowed)"
NIXPKGS_ALLOW_INSECURE=1 nix build --impure \
  'github:NixOS/nixpkgs/nixos-26.05#python2' -o "$WORK/py2" >/dev/null
PY2="$(readlink -f "$WORK/py2")/bin/python2.7"

if [ ! -d "$WORK/caldavtester/.git" ]; then
    echo "==> cloning CalDAVTester @ ${CTESTER_REF:0:12}"
    git clone https://github.com/apple/ccs-caldavtester "$WORK/caldavtester"
    git -C "$WORK/caldavtester" checkout -q "$CTESTER_REF"
fi
[ -d "$WORK/pycalendar/.git" ] || git clone --depth 1 https://github.com/apple/ccs-pycalendar.git "$WORK/pycalendar"

echo "==> tailoring serverinfo.xml (full feature list)"
git -C "$WORK/caldavtester" checkout -- scripts/server/serverinfo.xml
"$PY2" "$HERE/serverinfo-overrides.py" --full \
    "$WORK/caldavtester/scripts/server/serverinfo.xml" 127.0.0.1 "$PORT"

"$ROOT/scripts/dev-db.sh" start >/dev/null
echo "==> building calserver"
go build -o "$WORK/calserver" ./cmd/calserver

export PYTHONPATH="$WORK/pycalendar/src:$WORK/caldavtester"
cd "$WORK/caldavtester"
(cd scripts/tests && find . -name '*.xml' | sed 's|^\./||' | sort) > "$WORK/files.txt"
total=$(wc -l < "$WORK/files.txt" | tr -d ' ')
RES="$WORK/results-full.tsv"; : > "$RES"
echo "==> running $total files (fresh server each)"

to=0; tf=0; ti=0; te=0
pass=0; partial=0; failc=0; errc=0; skip=0; crash=0
while IFS= read -r f; do
    [ -z "$f" ] && continue
    psql -q -d "$PGDATABASE" -f "$HERE/seed.sql" >/dev/null 2>&1
    "$WORK/calserver" -addr "127.0.0.1:$PORT" -db "$DSN" -realm SabreDAV \
        -browser -auto-schedule-prefixes resource,location -principals-readable -auto-provision >"$WORK/srv.log" 2>&1 &
    SRV=$!
    sleep 0.8
    out="$(timeout 90 "$PY2" testcaldav.py "$f" 2>/dev/null)"
    if kill -0 $SRV 2>/dev/null; then alive=1; kill $SRV 2>/dev/null; else alive=0; fi
    wait $SRV 2>/dev/null
    s="$(printf '%s' "$out" | grep -E '\(ok=' | tail -1)"
    ok=$(printf '%s' "$s" | sed -nE 's/.*ok=([0-9]+).*/\1/p'); ok=${ok:-0}
    fa=$(printf '%s' "$s" | sed -nE 's/.*failed=([0-9]+).*/\1/p'); fa=${fa:-0}
    ig=$(printf '%s' "$s" | sed -nE 's/.*ignored=([0-9]+).*/\1/p'); ig=${ig:-0}
    er=$(printf '%s' "$s" | sed -nE 's/.*errors=([0-9]+).*/\1/p'); er=${er:-0}
    to=$((to+ok)); tf=$((tf+fa)); ti=$((ti+ig)); te=$((te+er))
    if   [ "$alive" -eq 0 ]; then cat=CRASH; crash=$((crash+1))
    elif [ -z "$s" ]; then cat=SKIP; skip=$((skip+1))
    elif [ "$ok" -gt 0 ] && [ "$fa" -eq 0 ] && [ "$er" -eq 0 ]; then cat=PASS; pass=$((pass+1))
    elif [ "$ok" -gt 0 ]; then cat=PARTIAL; partial=$((partial+1))
    elif [ "$er" -gt 0 ] && [ "$fa" -eq 0 ]; then cat=ERROR; errc=$((errc+1))
    elif [ "$fa" -eq 0 ] && [ "$er" -eq 0 ]; then cat=SKIP; skip=$((skip+1))
    else cat=FAIL; failc=$((failc+1)); fi
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$f" "$ok" "$fa" "$ig" "$er" "$cat" >> "$RES"
done < "$WORK/files.txt"

echo
echo "=========== FULL CalDAVTester SWEEP ==========="
printf "files: %s   PASS=%s PARTIAL=%s FAIL=%s ERROR=%s SKIP=%s CRASH=%s\n" \
    "$total" "$pass" "$partial" "$failc" "$errc" "$skip" "$crash"
printf "test cases: ok=%s failed=%s ignored=%s\n" "$to" "$tf" "$ti"
echo "per-file detail: $RES"
echo
echo "PASS + PARTIAL (the applicable, standards-based surface):"
awk -F'\t' '$6=="PASS"||$6=="PARTIAL"{printf "  %-42s %s/%s\n",$1,$2,$2+$3}' "$RES"
[ "$crash" -gt 0 ] && { echo; echo "CRASH (server died — investigate):"; awk -F'\t' '$6=="CRASH"{print "  "$1}' "$RES"; }
