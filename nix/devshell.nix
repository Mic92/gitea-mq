{
  mkShell,
  sqlc,
  golangci-lint,
  gitea-mq,
}:
mkShell {
  inputsFrom = [ gitea-mq ];
  packages = [
    sqlc
    golangci-lint
  ];
}
