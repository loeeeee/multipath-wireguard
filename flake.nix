{
  description = "multipath-wireguard: duplicate a WireGuard link across multiple tunnels for redundancy";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
      in
      {
        # Build: `nix build` -> ./result/bin/multipath-wireguard
        packages.default = pkgs.buildGoModule {
          pname = "multipath-wireguard";
          version = "0.1.0";
          src = ./.;

          # Stdlib only: there are no Go dependencies to vendor (see AGENTS.md rule 1).
          vendorHash = null;

          # Pure-Go static binary; the relay never needs cgo.
          env.CGO_ENABLED = "0";

          # Run `go test` as part of the build. Race tests (which need cgo) are
          # run in the dev shell, not the packaged build.
          doCheck = true;

          ldflags = [ "-s" "-w" ];

          meta = {
            description = "Duplicate a WireGuard link across multiple tunnels for redundancy";
            mainProgram = "multipath-wireguard";
          };
        };

        # `nix run . -- client -listen ...`
        apps.default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/multipath-wireguard";
        };

        # `nix develop` -> pinned Go plus tooling.
        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
            pkgs.gotools # goimports, etc.
            pkgs.go-tools # staticcheck
          ];
        };

        # `nix flake check` builds and runs tests via the package's checkPhase.
        checks.default = self.packages.${system}.default;

        formatter = pkgs.nixpkgs-fmt;
      });
}
