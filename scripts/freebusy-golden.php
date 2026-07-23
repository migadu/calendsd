<?php
/**
 * Emits sabre/vobject's FreeBusyGenerator output for the given .ics fixtures
 * over a fixed range, to pin the Go generator's semantics:
 *
 *   php scripts/freebusy-golden.php internal/caldav/ical/testdata/fb-*.ics
 */

require __DIR__.'/../dav/vendor/autoload.php';

date_default_timezone_set('UTC');

$objects = [];
foreach (array_slice($argv, 1) as $path) {
    $objects[] = file_get_contents($path);
}

$generator = new Sabre\VObject\FreeBusyGenerator();
$generator->setObjects($objects);
$generator->setTimeRange(
    new DateTime('2026-06-01T00:00:00Z'),
    new DateTime('2026-06-08T00:00:00Z')
);
$generator->setTimeZone(new DateTimeZone('UTC'));
echo $generator->getResult()->serialize();
