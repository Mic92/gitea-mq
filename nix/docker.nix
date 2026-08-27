{
  lib,
  pkgs,
  gitea-mq,
  imageName ? "${gitea-mq.pname}:latest",
}:
let
  allPlatforms = {
    "x86_64-linux" = {
      GOOS = "linux";
      GOARCH = "amd64";
    };
    "aarch64-linux" = {
      GOOS = "linux";
      GOARCH = "arm64";
    };
  };
  platforms = lib.mapAttrs (
    crossSystem:
    { GOOS, GOARCH }:
    let
      inherit (pkgs.stdenv.hostPlatform) system;
      crossPkgs =
        if system == crossSystem then pkgs else (import pkgs.path { inherit system crossSystem; });
    in
    crossPkgs.dockerTools.buildLayeredImage {
      name = gitea-mq.pname;
      tag = "${gitea-mq.version}-${crossSystem}";
      # The per-arch tarball is an intermediate that regctl decompresses
      # immediately; pigz has been observed to segfault on busy CI runners.
      # The final multi-arch image is compressed by regctl on export.
      compressor = "none";
      contents = [
        # Cross-compile with the native (cached) Go toolchain instead of
        # pulling in a full cross stdenv for Go.
        (gitea-mq.overrideAttrs (old: {
          env = (old.env or { }) // {
            inherit GOOS GOARCH;
            CGO_ENABLED = 0;
          };
          # Tests need a native postgres/gitea and can't run foreign binaries.
          doCheck = false;
          postInstall = (old.postInstall or "") + ''
            if [ -d $out/bin/${GOOS}_${GOARCH} ]; then
              mv $out/bin/${GOOS}_${GOARCH}/* $out/bin/
              rmdir $out/bin/${GOOS}_${GOARCH}
            fi
          '';
        }))
        # Merge branches are built by shelling out to git.
        crossPkgs.gitMinimal
      ]
      ++ (with crossPkgs.pkgsStatic; [
        busybox
        busybox-sandbox-shell
        cacert
      ]);
      config = {
        Entrypoint = [ "/bin/gitea-mq" ];
        Env = [
          "XDG_CACHE_HOME=/var/cache"
          "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
        ];
        Volumes = {
          "/var/cache/gitea-mq" = { };
        };
        ExposedPorts = {
          "8080" = { };
        };
      };
    }
  ) allPlatforms;
in
pkgs.stdenvNoCC.mkDerivation {
  name = "${gitea-mq.pname}-docker";
  inherit (gitea-mq) version;
  phases = [ "installPhase" ];
  src = pkgs.linkFarm "images" (lib.mapAttrsToList (name: path: { inherit name path; }) platforms);
  nativeBuildInputs = [ pkgs.regctl ];
  installPhase = ''
    set -xve
    image_refs=()
    for platform in $src/*; do
      ref_url="ocidir://images:$(basename $platform)"
      image_refs+=("--ref" "$ref_url")
      regctl image import "$ref_url" "$platform"
    done
    regctl index create "ocidir://images:latest" "''${image_refs[@]}"
    regctl image export "ocidir://images:latest" --name "${imageName}" > $out
  '';
}
