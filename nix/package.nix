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
  vendorHash = "sha256-Wsbaom3zPpZuyh5gG0DMvZ9Oo5nyIUSGa75E9qmZOC4=";
  nativeCheckInputs = [
    postgresql
    gitea
    git
  ];
  meta.mainProgram = "gitea-mq";
}
