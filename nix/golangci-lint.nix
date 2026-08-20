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
    GOMAXPROCS=$NIX_BUILD_CORES golangci-lint run --concurrency $NIX_BUILD_CORES
  '';
  installPhase = ''
    touch $out
  '';
})
