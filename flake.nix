{
  description = "calendsd — Go CalDAV/CardDAV server on Postgres";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };

        # PHP with the extensions the reference sabre/dav checkout needs
        # (pdo_pgsql for the shared-Postgres backend, plus its stock deps).
        php = pkgs.php83.buildEnv {
          extensions = { enabled, all }: enabled ++ (with all; [
            pdo_pgsql
            pgsql
          ]);
          extraConfig = ''
            memory_limit = 512M
          '';
        };
      in
      {
        devShells.default = pkgs.mkShell {
          packages = [
            # Go toolchain
            pkgs.go
            pkgs.gopls
            pkgs.gotools
            pkgs.golangci-lint
            pkgs.sqlc

            # Reference sabre/dav instance + corpus extraction
            php
            php.packages.composer

            # Shared database
            pkgs.postgresql_16

            # calweb end-to-end tests (Playwright). The browsers come from
            # nixpkgs (playwright-driver.browsers) rather than a download; the
            # @playwright/test npm version in cmd/calclient/web/e2e must match
            # pkgs.playwright-driver.version (see shellHook echo below).
            pkgs.nodejs
            pkgs.playwright-driver

          ]
          # WebDAV conformance suite (phase 1 gate) — not packaged for darwin;
          # run via Linux CI or build from source there.
          ++ pkgs.lib.optional pkgs.stdenv.isLinux pkgs.litmus;

          shellHook = ''
            export PGDATA="$PWD/.dev/pgdata"
            export PGHOST="$PWD/.dev/pg"
            export PGDATABASE=calserver
            export GOFLAGS=-mod=mod

            # Playwright: use the Nix-provided browsers instead of downloading
            # them, and skip host-requirement validation (non-NixOS hosts).
            export PLAYWRIGHT_BROWSERS_PATH="${pkgs.playwright-driver.browsers}"
            export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=true
            export PLAYWRIGHT_VERSION="${pkgs.playwright-driver.version}"
            echo "playwright-driver ${pkgs.playwright-driver.version} — pin @playwright/test to this in cmd/calclient/web/e2e/package.json"
          '';
        };
      });
}
