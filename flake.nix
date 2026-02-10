{
  description = "gitea-mq – merge queue for Gitea";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    flake-parts.inputs.nixpkgs-lib.follows = "nixpkgs";
    treefmt-nix.url = "github:numtide/treefmt-nix";
    treefmt-nix.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];

      imports = [ inputs.treefmt-nix.flakeModule ];

      flake.nixosModules.default = ./nix/module.nix;

      perSystem =
        {
          pkgs,
          self',
          lib,
          ...
        }:
        let
          gitea-mq = pkgs.callPackage ./nix/package.nix { };
        in
        {
          treefmt = {
            projectRootFile = "flake.nix";
            programs.nixfmt.enable = true;
            programs.gofumpt.enable = true;
          };

          packages.default = gitea-mq;
          packages.gitea-mq = gitea-mq;

          checks =
            let
              packages = lib.mapAttrs' (n: lib.nameValuePair "package-${n}") self'.packages;
              devShells = lib.mapAttrs' (n: lib.nameValuePair "devShell-${n}") self'.devShells;
            in
            {
              golangci-lint = pkgs.callPackage ./nix/golangci-lint.nix { inherit gitea-mq; };
            }
            // pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
              nixos-test = pkgs.callPackage ./nix/test.nix { self = inputs.self; };
            }
            // packages
            // devShells;

          devShells.default = pkgs.callPackage ./nix/devshell.nix { inherit gitea-mq; };
        };
    };
}
