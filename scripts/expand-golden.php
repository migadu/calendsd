<?php
/**
 * Emits sabre/vobject's expand() output for a fixture over a fixed range, to
 * pin the Go expansion's DST behavior:
 *
 *   php scripts/expand-golden.php internal/caldav/ical/testdata/dst-override.ics \
 *     > internal/caldav/ical/testdata/dst-override-expanded.ics
 */

require __DIR__.'/../dav/vendor/autoload.php';

date_default_timezone_set('UTC');

$vcal = Sabre\VObject\Reader::read(file_get_contents($argv[1]));
$expanded = $vcal->expand(
    new DateTime('2026-03-23T00:00:00Z'),
    new DateTime('2026-04-05T00:00:00Z'),
    new DateTimeZone('UTC')
);
echo $expanded->serialize();
