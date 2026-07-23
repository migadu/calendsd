<?php

/*
 * Reference sabre/dav CalDAV server for the co-existence harness (PLAN.md §5).
 * Runs against the shared dev Postgres (scripts/dev-db.sh) over its unix
 * socket. Start with:
 *
 *   php -S 127.0.0.1:8081 scripts/sabre-server.php
 *
 * Scope matches the MVP port target: auth, ACL, CalDAV, sync. Scheduling,
 * sharing and subscriptions stay off so both servers expose the same feature
 * set during parity testing.
 */

// The Go server assumes UTC (see internal/caldav/ical maxDate); the reference
// must agree or occurrence bounds diverge.
date_default_timezone_set('UTC');

require __DIR__.'/../dav/vendor/autoload.php';

$socketDir = getenv('PGHOST') ?: __DIR__.'/../.dev/pg';
$database = getenv('PGDATABASE') ?: 'calserver';
$pdo = new PDO(sprintf('pgsql:host=%s;dbname=%s', $socketDir, $database));
$pdo->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);

$authBackend = new Sabre\DAV\Auth\Backend\PDO($pdo);
$authBackend->setRealm('SabreDAV'); // baked into users.digesta1 — see PLAN.md §5
$calendarBackend = new Sabre\CalDAV\Backend\PDO($pdo);
$principalBackend = new Sabre\DAVACL\PrincipalBackend\PDO($pdo);

$tree = [
    new Sabre\CalDAV\Principal\Collection($principalBackend),
    new Sabre\CalDAV\CalendarRoot($principalBackend, $calendarBackend),
];

$server = new Sabre\DAV\Server($tree);
$server->setBaseUri('/');

$server->addPlugin(new Sabre\DAV\Auth\Plugin($authBackend));
$server->addPlugin(new Sabre\DAVACL\Plugin());
$server->addPlugin(new Sabre\CalDAV\Plugin());
$server->addPlugin(new Sabre\DAV\Sync\Plugin());
$server->addPlugin(new Sabre\DAV\Browser\Plugin());

$server->start();
