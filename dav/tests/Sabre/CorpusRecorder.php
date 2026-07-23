<?php

declare(strict_types=1);

namespace Sabre;

/**
 * Corpus extraction shim for the Go port (see ../PLAN.md §7).
 *
 * When the CORPUS_DIR environment variable is set, every request made through
 * AbstractDAVServerTestCase::request() is dumped as a JSON fixture record:
 * test name, server setup state, request, and the reference implementation's
 * response. The Go corpus runner replays these against the Go server.
 *
 * Inactive (no-op) when CORPUS_DIR is unset, so the sabre suite behaves
 * normally.
 *
 *   CORPUS_DIR=../testdata/corpus vendor/bin/phpunit tests/Sabre/CalDAV
 */
class CorpusRecorder
{
    private static int $seq = 0;

    /**
     * Captures the request before Server::exec() consumes its body stream.
     * Returns null when recording is disabled.
     */
    public static function snapshotRequest($request): ?array
    {
        if (!getenv('CORPUS_DIR')) {
            return null;
        }
        $body = $request->getBodyAsString();
        // Reading a stream body consumes it; restore so exec() sees it.
        $request->setBody($body);

        return [
            'method' => $request->getMethod(),
            'url' => $request->getUrl(),
            'headers' => $request->getHeaders(),
            'body' => $body,
        ];
    }

    public static function record(AbstractDAVServerTestCase $test, ?array $requestData, $response): void
    {
        $dir = getenv('CORPUS_DIR');
        if (!$dir || null === $requestData) {
            return;
        }
        if (!is_dir($dir)) {
            mkdir($dir, 0777, true);
        }

        $record = [
            'test' => get_class($test).'::'.$test->getName(false),
            'seq' => ++self::$seq,
            'setup' => self::setupState($test),
            'request' => $requestData,
            'response' => [
                'status' => $response->getStatus(),
                'headers' => $response->getHeaders(),
                'body' => $response->getBodyAsString(),
            ],
        ];

        $name = preg_replace('/[^A-Za-z0-9_.-]+/', '_', $record['test']).'__'.$record['seq'].'.json';
        file_put_contents(
            rtrim($dir, '/').'/'.$name,
            json_encode($record, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES | JSON_INVALID_UTF8_SUBSTITUTE)."\n"
        );
    }

    /**
     * Serializes the protected fixture state of the test case. Values that
     * cannot be represented in JSON are tagged so the Go runner can skip
     * records it cannot seed.
     */
    private static function setupState(AbstractDAVServerTestCase $test): array
    {
        $props = [
            'setupCalDAV', 'setupCardDAV', 'setupACL', 'setupCalDAVSharing',
            'setupCalDAVScheduling', 'setupCalDAVSubscriptions',
            'setupCalDAVICSExport', 'setupLocks', 'setupFiles',
            'setupPropertyStorage', 'autoLogin',
            'caldavCalendars', 'caldavCalendarObjects',
        ];
        $state = [];
        $ref = new \ReflectionClass(AbstractDAVServerTestCase::class);
        foreach ($props as $prop) {
            $p = $ref->getProperty($prop);
            $p->setAccessible(true);
            $state[$prop] = self::jsonable($p->getValue($test));
        }

        // The principal fixture set lives in the mock principal backend.
        $p = $ref->getProperty('principalBackend');
        $p->setAccessible(true);
        $principalBackend = $p->getValue($test);
        if ($principalBackend instanceof DAVACL\PrincipalBackend\Mock) {
            $state['principals'] = self::jsonable($principalBackend->getPrincipalsByPrefix('principals'));
        }

        return $state;
    }

    private static function jsonable($value)
    {
        if (is_array($value)) {
            return array_map([self::class, 'jsonable'], $value);
        }
        if ($value instanceof CalDAV\Xml\Property\SupportedCalendarComponentSet) {
            return ['__type' => 'supported-calendar-component-set', 'components' => $value->getValue()];
        }
        if ($value instanceof \DateTimeInterface) {
            return ['__type' => 'datetime', 'value' => $value->format(\DateTimeInterface::ATOM)];
        }
        if (is_object($value)) {
            return ['__type' => 'unserializable', 'class' => get_class($value)];
        }

        return $value;
    }
}
