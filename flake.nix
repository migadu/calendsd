{
  description = "calserver — Go CalDAV server co-existing with sabre/dav on one Postgres";

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

          ]
          # WebDAV conformance suite (phase 1 gate) — not packaged for darwin;
          # run via Linux CI or build from source there.
          ++ pkgs.lib.optional pkgs.stdenv.isLinux pkgs.litmus;

          shellHook = ''
            export PGDATA="$PWD/.dev/pgdata"
            export PGHOST="$PWD/.dev/pg"
            export PGDATABASE=calserver
            export GOFLAGS=-mod=mod
          '';
        };
      });
}
