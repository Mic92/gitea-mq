{
  mkShell,
  sqlc,
  golangci-lint,
  postgresql,
  gitea-mq,
}:
mkShell {
  inputsFrom = [ gitea-mq ];
  packages = [
    sqlc
    golangci-lint
    # testutil spawns a temporary postgres via initdb/postgres/pg_isready
    postgresql
  ];
}
