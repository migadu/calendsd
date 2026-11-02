# Security policy

## Reporting a vulnerability

Please do not open a public issue for security problems. Report them
privately through GitHub's
[private vulnerability reporting](https://github.com/migadu/calendsd/security/advisories/new)
(the "Report a vulnerability" button on the Security tab).

Please include what you found, how to reproduce it, and what an attacker could
do with it. We will acknowledge the report within a few working days, keep you
updated while we fix it, and credit you in the advisory unless you would
rather stay anonymous.

## Supported versions

Fixes go into the `main` branch and the next release. Older releases do not
receive backports.

## Scope

All three binaries (`calendsd`, `calctl`, `calclient`) and everything under
`internal/` are in scope. Pay particular attention to authentication,
access control between users, and anything reachable without logging in.

`dav/` is a sabre/dav checkout used only as a reference
and to extract test fixtures. It is not built into any binary, so report
issues in it [upstream](https://github.com/sabre-io/dav).

## Deployment notes

- Basic authentication (which the passwd-file and rcptd modes require) sends
  the password with every request: always serve calendsd over TLS.
- The metrics/pprof listener (`metrics_addr`), the contacts API (`api_addr`)
  and the admin panel (`calctl panel`) are meant for loopback or a trusted
  network only. Never expose them publicly.
