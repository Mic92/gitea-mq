{
  lib,
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
  inherit vendorHash;
  nativeCheckInputs = [
    postgresql
    gitea
    git
  ];
  meta.mainProgram = "gitea-mq";
}
