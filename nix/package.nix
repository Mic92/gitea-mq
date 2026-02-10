{
  lib,
  buildGoModule,
  postgresql,
  gitea,
  git,
}:
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
  vendorHash = "sha256-yvZ9HaoNHL47FBEDAtnWxm3o+8t+uYUrkVdfypJjQVw=";
  nativeCheckInputs = [
    postgresql
    gitea
    git
  ];
  meta.mainProgram = "gitea-mq";
}
