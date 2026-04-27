{
  description = "gitea-mq – merge queue for Gitea";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    treefmt-nix.url = "github:numtide/treefmt-nix";
    treefmt-nix.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    {
      self,
      nixpkgs,
      treefmt-nix,
    }:
    let
      inherit (nixpkgs) lib;
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      eachSystem = lib.genAttrs systems;
      pkgsFor = system: nixpkgs.legacyPackages.${system};

      treefmtFor =
        system:
        treefmt-nix.lib.evalModule (pkgsFor system) {
          projectRootFile = "flake.nix";
          programs.nixfmt.enable = true;
          programs.gofumpt.enable = true;
        };
    in
    {
      nixosModules.default = ./nix/module.nix;

      packages = eachSystem (
        system:
        let
          pkgs = pkgsFor system;
          gitea-mq = pkgs.callPackage ./nix/package.nix { };
        in
        {
          inherit gitea-mq;
          default = gitea-mq;
        }
      );

      devShells = eachSystem (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          default = pkgs.callPackage ./nix/devshell.nix {
            inherit (self.packages.${system}) gitea-mq;
          };
        }
      );

      formatter = eachSystem (system: (treefmtFor system).config.build.wrapper);

      checks = eachSystem (
        system:
        let
          pkgs = pkgsFor system;
          packages = lib.mapAttrs' (n: lib.nameValuePair "package-${n}") self.packages.${system};
          devShells = lib.mapAttrs' (n: lib.nameValuePair "devShell-${n}") self.devShells.${system};
        in
        {
          formatting = (treefmtFor system).config.build.check self;
          golangci-lint = pkgs.callPackage ./nix/golangci-lint.nix {
            inherit (self.packages.${system}) gitea-mq;
          };
        }
        // lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          nixos-test = pkgs.callPackage ./nix/test.nix { inherit self; };
        }
        // packages
        // devShells
      );
    };
}
