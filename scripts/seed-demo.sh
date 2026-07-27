#!/usr/bin/env bash
# Seeds demo data into the running dev calserver, focused on bob's account so
# calweb has a populated calendar and contacts on first login (bob/bob):
# a Work and a Private calendar with events (incl. a recurrence and a
# multi-day all-day event), a Contacts book, and one pending invitation sent
# from admin. Idempotent: safe to re-run (PUTs overwrite, MKs tolerate
# "already exists").
#
#   make seed            # against http://127.0.0.1:8080
#   BASE=http://... scripts/seed-demo.sh
set -euo pipefail

cd "$(dirname "$0")/.."
: "${BASE:=http://127.0.0.1:8080}"
: "${PGHOST:=$PWD/.dev/pg}"
: "${PGDATABASE:=calserver}"
export PGHOST PGDATABASE

# AS holds the current account's credentials; helpers below act as that user.
AS="bob:bob"

req() { # req METHOD PATH [curl args...] -> prints http code
    local method=$1 path=$2; shift 2
    curl -s -o /dev/null -w '%{http_code}' -u "$AS" -X "$method" "$BASE$path" "$@"
}

expect() { # expect "label" got want...
    local label=$1 got=$2; shift 2
    for want in "$@"; do
        if [ "$got" = "$want" ]; then echo "  ✓ $label ($got)"; return 0; fi
    done
    echo "  ✗ $label: got HTTP $got (wanted $*)" >&2
    return 1
}

mkcal() { # mkcal PATH DISPLAYNAME COLOR
    local path=$1 dn=$2 color=$3
    local code
    code=$(req MKCALENDAR "$path" -H 'Content-Type: application/xml' --data "<?xml version=\"1.0\" encoding=\"utf-8\"?>
<c:mkcalendar xmlns:d=\"DAV:\" xmlns:c=\"urn:ietf:params:xml:ns:caldav\"><d:set><d:prop>
<d:displayname>$dn</d:displayname>
<c:supported-calendar-component-set><c:comp name=\"VEVENT\"/><c:comp name=\"VTODO\"/></c:supported-calendar-component-set>
</d:prop></d:set></c:mkcalendar>")
    expect "MKCALENDAR $path" "$code" 201 405   # 405: already exists
    code=$(req PROPPATCH "$path" -H 'Content-Type: application/xml' --data "<?xml version=\"1.0\" encoding=\"utf-8\"?>
<d:propertyupdate xmlns:d=\"DAV:\" xmlns:a=\"http://apple.com/ns/ical/\">
<d:set><d:prop><a:calendar-color>$color</a:calendar-color></d:prop></d:set></d:propertyupdate>")
    expect "PROPPATCH color $path" "$code" 207
}

put_ics() { # put_ics PATH ICS-LINES...
    local path=$1; shift
    local body code
    # $() strips the final newline, leaving a bare \r — add the \n back.
    body=$(printf '%s\r\n' "$@")$'\n'
    code=$(req PUT "$path" -H 'Content-Type: text/calendar; charset=utf-8' --data-binary "$body")
    expect "PUT $path" "$code" 201 204
}

put_vcf() { # put_vcf PATH VCF-LINES...
    local path=$1; shift
    local body code
    body=$(printf '%s\r\n' "$@")$'\n'
    code=$(req PUT "$path" -H 'Content-Type: text/vcard; charset=utf-8' --data-binary "$body")
    expect "PUT $path" "$code" 201 204
}

if ! curl -s -o /dev/null "$BASE/"; then
    echo "no server at $BASE — start it first (make dev)" >&2
    exit 1
fi

echo "seeding bob principal (users + principal + proxy rows)"
psql -q -v ON_ERROR_STOP=1 -d "$PGDATABASE" <<'SQL'
INSERT INTO users (username, digesta1)
SELECT 'bob', '40d17625c1ef3974fc1138a86b8248fc'
WHERE NOT EXISTS (SELECT 1 FROM users WHERE username = 'bob');
INSERT INTO principals (uri, email, displayname)
SELECT v.uri, v.email, v.dn FROM (VALUES
    ('principals/bob', 'bob@example.org', 'Bob'),
    ('principals/bob/calendar-proxy-read',  NULL, NULL),
    ('principals/bob/calendar-proxy-write', NULL, NULL)
) AS v(uri, email, dn)
WHERE NOT EXISTS (SELECT 1 FROM principals WHERE principals.uri = v.uri);
SQL

# Touch bob's homes so -auto-provision creates his default calendar/addressbook.
req PROPFIND /calendars/bob/ -H 'Depth: 0' >/dev/null || true
req PROPFIND /addressbooks/bob/ -H 'Depth: 0' >/dev/null || true

today=$(date -u +%Y%m%d)
tomorrow=$(date -u -v+1d +%Y%m%d 2>/dev/null || date -u -d '+1 day' +%Y%m%d)
plus4=$(date -u -v+4d +%Y%m%d 2>/dev/null || date -u -d '+4 day' +%Y%m%d)
stamp="${today}T120000Z"

echo "== bob's calendars"
mkcal /calendars/bob/work/    'Work'    '#3174adff'
mkcal /calendars/bob/private/ 'Private' '#3a8a5fff'

echo "== bob's events"
put_ics /calendars/bob/work/seed-meeting.ics \
    'BEGIN:VCALENDAR' 'VERSION:2.0' 'PRODID:-//calclient-seed//EN' \
    'BEGIN:VEVENT' 'UID:seed-meeting' "DTSTAMP:$stamp" \
    "DTSTART:${tomorrow}T090000Z" "DTEND:${tomorrow}T100000Z" \
    'SUMMARY:Team meeting' 'LOCATION:Room 3' 'DESCRIPTION:Weekly sync' \
    'END:VEVENT' 'END:VCALENDAR'

put_ics /calendars/bob/work/seed-standup.ics \
    'BEGIN:VCALENDAR' 'VERSION:2.0' 'PRODID:-//calclient-seed//EN' \
    'BEGIN:VEVENT' 'UID:seed-standup' "DTSTAMP:$stamp" \
    "DTSTART:${tomorrow}T083000Z" "DTEND:${tomorrow}T084500Z" \
    'SUMMARY:Daily standup' 'RRULE:FREQ=DAILY;COUNT=10' \
    'END:VEVENT' 'END:VCALENDAR'

# Multi-day all-day event → renders as a spanning bar in the month grid.
put_ics /calendars/bob/work/seed-offsite.ics \
    'BEGIN:VCALENDAR' 'VERSION:2.0' 'PRODID:-//calclient-seed//EN' \
    'BEGIN:VEVENT' 'UID:seed-offsite' "DTSTAMP:$stamp" \
    "DTSTART;VALUE=DATE:$tomorrow" "DTEND;VALUE=DATE:$plus4" \
    'SUMMARY:Team offsite' 'LOCATION:Bern' \
    'END:VEVENT' 'END:VCALENDAR'

put_ics /calendars/bob/private/seed-dentist.ics \
    'BEGIN:VCALENDAR' 'VERSION:2.0' 'PRODID:-//calclient-seed//EN' \
    'BEGIN:VEVENT' 'UID:seed-dentist' "DTSTAMP:$stamp" \
    "DTSTART:${tomorrow}T160000Z" "DTEND:${tomorrow}T164500Z" \
    'SUMMARY:Dentist' 'LOCATION:Bahnhofstrasse 12' \
    'END:VEVENT' 'END:VCALENDAR'

put_ics /calendars/bob/private/seed-gym.ics \
    'BEGIN:VCALENDAR' 'VERSION:2.0' 'PRODID:-//calclient-seed//EN' \
    'BEGIN:VEVENT' 'UID:seed-gym' "DTSTAMP:$stamp" \
    "DTSTART:${today}T173000Z" "DTEND:${today}T183000Z" \
    'SUMMARY:Gym' 'RRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;COUNT=12' \
    'END:VEVENT' 'END:VCALENDAR'

echo "== bob's contacts"
code=$(req MKCOL /addressbooks/bob/contacts/ -H 'Content-Type: application/xml' --data '<?xml version="1.0" encoding="utf-8"?>
<d:mkcol xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:set><d:prop>
<d:resourcetype><d:collection/><card:addressbook/></d:resourcetype>
<d:displayname>Contacts</d:displayname></d:prop></d:set></d:mkcol>')
expect "MKCOL bob contacts" "$code" 201 405

put_vcf /addressbooks/bob/contacts/seed-alice.vcf \
    'BEGIN:VCARD' 'VERSION:3.0' 'UID:seed-alice' \
    'FN:Alice Muster' 'N:Muster;Alice;;;' \
    'EMAIL;TYPE=INTERNET:alice@example.org' 'TEL;TYPE=VOICE:+41 44 765 43 21' \
    'ORG:Muster AG' 'TITLE:Product Manager' \
    'END:VCARD'

put_vcf /addressbooks/bob/contacts/seed-carol.vcf \
    'BEGIN:VCARD' 'VERSION:3.0' 'UID:seed-carol' \
    'FN:Carol Meier' 'N:Meier;Carol;;;' \
    'EMAIL;TYPE=INTERNET;TYPE=WORK:carol.meier@example.org' \
    'EMAIL;TYPE=INTERNET;TYPE=HOME:carol@example.net' \
    'TEL;TYPE=CELL:+41 79 234 56 78' 'ORG:Example GmbH' \
    'END:VCARD'

put_vcf /addressbooks/bob/contacts/seed-david.vcf \
    'BEGIN:VCARD' 'VERSION:3.0' 'UID:seed-david' \
    'FN:David Schneider' 'N:Schneider;David;;;' \
    'EMAIL;TYPE=INTERNET:david.schneider@example.org' \
    'TEL;TYPE=VOICE:+41 31 987 65 43' 'ADR;TYPE=WORK:;;Bahnhofstrasse 1;Bern;;3000;Switzerland' \
    'END:VCARD'

put_vcf /addressbooks/bob/contacts/seed-erika.vcf \
    'BEGIN:VCARD' 'VERSION:3.0' 'UID:seed-erika' \
    'FN:Erika Weber' 'N:Weber;Erika;;;' \
    'EMAIL;TYPE=INTERNET:erika.weber@example.org' \
    'TEL;TYPE=CELL:+41 76 345 67 89' 'ORG:Weber & Co' 'TITLE:Architect' \
    'END:VCARD'

put_vcf /addressbooks/bob/contacts/seed-frank.vcf \
    'BEGIN:VCARD' 'VERSION:3.0' 'UID:seed-frank' \
    'FN:Frank Keller' 'N:Keller;Frank;;;' \
    'EMAIL;TYPE=INTERNET:frank.keller@example.org' 'TEL;TYPE=VOICE:+41 44 111 22 33' \
    'END:VCARD'

# One pending invitation for bob: admin sends an event with bob as ATTENDEE,
# which the scheduling plugin delivers to /calendars/bob/inbox/.
echo "== invitation from admin → bob"
AS="admin:admin"
put_ics /calendars/admin/default/seed-invite.ics \
    'BEGIN:VCALENDAR' 'VERSION:2.0' 'PRODID:-//calclient-seed//EN' \
    'BEGIN:VEVENT' 'UID:seed-invite' "DTSTAMP:$stamp" \
    "DTSTART:${tomorrow}T140000Z" "DTEND:${tomorrow}T150000Z" \
    'SUMMARY:Planning with Admin' 'LOCATION:HQ' \
    'ORGANIZER;CN=admin@example.org:mailto:admin@example.org' \
    'ATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:bob@example.org' \
    'END:VEVENT' 'END:VCALENDAR'
AS="bob:bob"

echo
echo "done. Sign in to calweb (/cal/) as bob / bob:"
echo "  • Work calendar    → Team meeting, Daily standup (recurring),"
echo "                       and 'Team offsite' as a multi-day bar."
echo "  • Private calendar → Dentist, and a Mon/Wed/Fri Gym series."
echo "  • Contacts         → Alice, Carol, David, Erika, Frank."
echo "  • Invitations (✉)  → 'Planning with Admin' to Accept/Maybe/Decline."
