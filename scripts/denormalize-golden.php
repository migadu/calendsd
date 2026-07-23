<?php
/**
 * Emits sabre's getDenormalizedData() output for each .ics fixture on the
 * command line, as JSON keyed by filename. Used to cross-check the Go
 * implementation in internal/caldav/ical against the reference:
 *
 *   php scripts/denormalize-golden.php internal/caldav/ical/testdata/*.ics \
 *     > internal/caldav/ical/testdata/golden.json
 */

require __DIR__.'/../dav/vendor/autoload.php';

date_default_timezone_set('UTC');

// getDenormalizedData is protected; a PDO-free subclass exposes it without
// needing a database connection.
class Denormalizer extends Sabre\CalDAV\Backend\PDO
{
    public function __construct()
    {
    }

    public function denormalize(string $data): array
    {
        return $this->getDenormalizedData($data);
    }
}

$d = new Denormalizer();
$out = [];
foreach (array_slice($argv, 1) as $path) {
    $out[basename($path)] = $d->denormalize(file_get_contents($path));
}
echo json_encode($out, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES), "\n";
