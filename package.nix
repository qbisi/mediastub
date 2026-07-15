{
  lib,
  buildGoModule,
}:

buildGoModule {
  pname = "mediastub";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.unions [
      ./cmd
      ./core
      ./internal
      ./marker
      ./mountfs
      ./origin
      ./pathfilter
      ./syncer
      ./testdata
      ./go.mod
      ./go.sum
      ./COPYING
      ./THIRD_PARTY_NOTICES
    ];
  };

  vendorHash = "sha256-RsN9EkoYa5N7SPmgv52oMG4C7CWYASVyE9uJxPdBkFE=";
  subPackages = [ "cmd/mediastub" ];
  env.CGO_ENABLED = 0;

  checkPhase = ''
    runHook preCheck
    go test ./...
    runHook postCheck
  '';

  meta = {
    description = "Metadata-only media views and sidecar synchronization";
    homepage = "https://github.com/qbisi/mediastub";
    license = lib.licenses.mit;
    mainProgram = "mediastub";
    platforms = lib.platforms.linux;
  };
}
