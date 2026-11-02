# Strangler-fig migration: routing traffic from sabre to calendsd

> **Historical.** The migration is complete and sabre is no longer a rollback
> target: calendsd may add tables and columns sabre does not know about
> (`internal/schema/migrations/`). Kept as a record of how the cutover was run.

Both servers run against the same Postgres database (docs/design/PLAN.md §5), so the Go
server can take over incrementally: front both with a reverse proxy and move
routes over as confidence grows. The live parity test
(`PARITY=1 go test ./internal/storage/ -run TestParity`) gates each routing
change.

## Constraints

- **Auth must be indistinguishable on both sides.** Both servers verify
  against `users.digesta1` with the same realm (the realm is baked into the
  hash), so credentials work identically. Digest nonces are not shared
  between the servers; clients renegotiate transparently on a 401, but if
  you see auth loops, route per *path prefix* rather than per method so one
  client session stays on one backend.
- **`MOVE`/`COPY` carry a `Destination` header** — source and destination
  must land on the same backend. Both are routed with the writes below.
- Roll forward read-only first, then writes, then decommission sabre.

## Stage 1 — reads to Go, writes to sabre (Caddy)

```caddyfile
example.com {
    @reads method GET HEAD OPTIONS PROPFIND REPORT
    handle /.well-known/caldav {
        redir / 301
    }
    handle @reads {
        reverse_proxy calendsd:8080
    }
    handle {
        # PUT, DELETE, PROPPATCH, MKCOL, MKCALENDAR, COPY, MOVE, ACL
        reverse_proxy sabre-php:80
    }
}
```

nginx equivalent of the method split:

```nginx
map $request_method $calendar_backend {
    default            http://sabre-php:80;
    GET                http://calendsd:8080;
    HEAD               http://calendsd:8080;
    OPTIONS            http://calendsd:8080;
    PROPFIND           http://calendsd:8080;
    REPORT             http://calendsd:8080;
}
server {
    location /.well-known/caldav { return 301 /; }
    location / { proxy_pass $calendar_backend; }
}
```

## Stage 2 — everything to Go

```caddyfile
example.com {
    reverse_proxy calendsd:8080
}
```

Keep sabre running (not routed) for one sync cycle as a rollback target; the
shared database means flipping back is a proxy change, not a migration.
