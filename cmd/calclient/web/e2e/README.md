# calweb end-to-end tests (Playwright)

Drives the real calweb UI in a browser against a live calserver + calclient,
which `global-setup.js` builds from source and runs against the shared dev
Postgres. calclient serves the working tree (`-dev-assets`), so the tests
exercise your local edits with no rebuild of the web assets.

## Prerequisites

Everything comes from the flake's devShell — Go, Node, and the Playwright
browsers (via `pkgs.playwright-driver`, no download):

    nix develop            # from the calserver module root; sets PLAYWRIGHT_BROWSERS_PATH
    scripts/dev-db.sh start # the shared dev Postgres the setup builds against

The browsers are pinned by Nix, so `@playwright/test` in package.json must match
`pkgs.playwright-driver.version` (the devShell prints it on entry; currently
1.61.1). Bump both together when you update nixpkgs.

## Run

    nix develop
    cd cmd/calclient/web/e2e
    npm install              # installs @playwright/test only — NOT browsers
    npm test                 # headless
    npm run test:headed      # watch it drive the browser
    npm run report           # open the last HTML report

Login used by the specs: `bob` / `bob` (dev DB).

## Cross-browser

The event editor no longer uses native `<input type="datetime-local">` for
times — every engine formats that per the browser/OS locale (Chromium ignores
the element's `lang`), which broke the 24-hour preference. Times are now an
app-formatted text field, so their format is engine-independent and asserted
directly in `edit-dialog.spec.js`. The Nix `playwright-driver.browsers` bundle
still contains webkit and firefox — uncomment those projects in
`playwright.config.js` to check dialog behaviour across engines (no extra
install needed).

## Layout

    playwright.config.js   config; server lifecycle via global setup/teardown
    global-setup.js        build + start calserver/calclient, wait for ready
    global-teardown.js     stop them
    tests/helpers.js       login + settings helpers
    tests/*.spec.js        specs (edit dialog, grid/now-line, free-busy)
