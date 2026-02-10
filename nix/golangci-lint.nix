{
  golangci-lint,
  gitea-mq,
}:
gitea-mq.overrideAttrs (old: {
  nativeBuildInputs = old.nativeBuildInputs ++ [ golangci-lint ];
  outputs = [ "out" ];
  buildPhase = ''
    HOME=$TMPDIR
    golangci-lint run
  '';
  installPhase = ''
    touch $out
  '';
})
