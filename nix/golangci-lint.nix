{
  golangci-lint,
  gitea-mq,
}:
gitea-mq.overrideAttrs (old: {
  nativeBuildInputs = old.nativeBuildInputs ++ [ golangci-lint ];
  outputs = [ "out" ];
  # Pure-Go project: keep the linter from forking gcc for cgo typechecks.
  env.CGO_ENABLED = "0";
  buildPhase = ''
    HOME=$TMPDIR
    golangci-lint run
  '';
  installPhase = ''
    touch $out
  '';
})
