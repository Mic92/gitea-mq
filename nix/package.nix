{
  lib,
  stdenv,
  buildGoModule,
  postgresql,
  gitea,
  git,
}:
let
  vendorHash = lib.fileContents ./goVendorHash.txt;
in
buildGoModule {
  pname = "gitea-mq";
  version = "0.1.0";
  src = lib.fileset.toSource {
    root = ./..;
    fileset = lib.fileset.unions [
      ./../go.mod
      ./../go.sum
      ./../cmd
      ./../internal
    ];
  };
  # Don't run E2E tests on macOS
  subPackages = lib.optionals stdenv.hostPlatform.isDarwin [ "cmd/gitea-mq" ];
  inherit vendorHash;
  nativeCheckInputs = lib.optionals stdenv.hostPlatform.isLinux [
    postgresql
    gitea
    git
  ];
  meta.mainProgram = "gitea-mq";
}
