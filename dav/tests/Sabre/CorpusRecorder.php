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
     * Captures the request before Server::exec() consumes its body stream,
     * plus the DAV tree state at this moment (pre-request, so replaying a
     * test's records in sequence can seed from the first record's tree).
     * Returns null when recording is disabled.
     */
    public static function snapshotRequest(AbstractDAVServerTestCase $test, $request): ?array
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
            'tree' => self::serializeTree($test),
        ];
    }

    /**
     * Serializes the test's DAV tree. Only sabre's Mock node types are
     * representable; anything else is tagged so the Go runner knows the
     * record needs backend-based seeding (or cannot be replayed yet).
     */
    private static function serializeTree(AbstractDAVServerTestCase $test)
    {
        $ref = new \ReflectionClass(AbstractDAVServerTestCase::class);
        $p = $ref->getProperty('tree');
        $p->setAccessible(true);
        $tree = $p->getValue($test);
        if (is_array($tree)) {
            return ['type' => 'roots', 'roots' => array_map([self::class, 'serializeNode'], $tree)];
        }

        return self::serializeNode($tree);
    }

    private static function serializeNode($node): array
    {
        if ($node instanceof DAV\Mock\PropertiesCollection) {
            return [
                'type' => 'propscollection',
                'name' => $node->getName(),
                'failMode' => $node->failMode,
                'properties' => self::jsonable($node->properties),
                'children' => array_map([self::class, 'serializeNode'], $node->getChildren()),
            ];
        }
        if ($node instanceof DAV\Mock\Collection) {
            return [
                'type' => 'collection',
                'name' => $node->getName(),
                'children' => array_map([self::class, 'serializeNode'], $node->getChildren()),
            ];
        }
        if ($node instanceof DAV\Sync\MockSyncCollection) {
            $changeLog = [];
            foreach ($node->changeLog as $token => $change) {
                $changeLog[] = [
                    'token' => $token,
                    'added' => $change['added'],
                    'modified' => $change['modified'],
                    'deleted' => $change['deleted'],
                ];
            }

            return [
                'type' => 'synccollection',
                'name' => $node->getName(),
                'token' => $node->token,
                'changeLog' => $changeLog,
                'children' => array_map([self::class, 'serializeNode'], $node->getChildren()),
            ];
        }
        if ($node instanceof \Sabre\DAV\SimpleFile) {
            return [
                'type' => 'simplefile',
                'name' => $node->getName(),
                'contents' => $node->get(),
            ];
        }
        if ($node instanceof \Sabre\DAV\SimpleCollection) {
            return [
                'type' => 'collection',
                'name' => $node->getName(),
                'children' => array_map([self::class, 'serializeNode'], $node->getChildren()),
            ];
        }
        if ($node instanceof DAV\Mock\StreamingFile) {
            $ref = new \ReflectionClass(DAV\Mock\File::class);
            $p = $ref->getProperty('contents');
            $p->setAccessible(true);
            $contents = $p->getValue($node);
            if (is_resource($contents)) {
                $pos = ftell($contents);
                rewind($contents);
                $data = stream_get_contents($contents);
                fseek($contents, $pos);
            } else {
                $data = $contents;
            }

            return [
                'type' => 'streamingfile',
                'name' => $node->getName(),
                'contents' => $data,
                'size' => $node->getSize(),
            ];
        }
        if ($node instanceof DAV\Mock\File) {
            return [
                'type' => 'file',
                'name' => $node->getName(),
                'contents' => $node->get(),
                'lastModified' => $node->getLastModified(),
            ];
        }

        return ['type' => 'node', 'class' => get_class($node), 'name' => $node->getName()];
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
