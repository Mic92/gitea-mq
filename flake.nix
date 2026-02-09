{
  description = "gitea-mq – merge queue for Gitea";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    treefmt-nix.url = "github:numtide/treefmt-nix";
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

      perSystem =
        { pkgs, self', ... }:
        {
          treefmt = {
            projectRootFile = "flake.nix";
            programs.nixfmt.enable = true;
            programs.gofmt.enable = true;
          };

          packages.default = self'.packages.gitea-mq;

          packages.gitea-mq = pkgs.buildGoModule {
            pname = "gitea-mq";
            version = "0.1.0";
            src = pkgs.lib.fileset.toSource {
              root = ./.;
              fileset = pkgs.lib.fileset.unions [
                ./go.mod
                ./go.sum
                ./cmd
                ./internal
              ];
            };
            vendorHash = "sha256-Wsbaom3zPpZuyh5gG0DMvZ9Oo5nyIUSGa75E9qmZOC4=";
          };

          devShells.default = pkgs.mkShell {
            inputsFrom = [ self'.packages.gitea-mq ];
            packages = with pkgs; [
              sqlc
              golangci-lint
            ];
          };
        };
    };
}
