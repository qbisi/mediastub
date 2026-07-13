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
      ./mountfs
      ./origin
      ./testdata
      ./go.mod
      ./go.sum
      ./COPYING
      ./THIRD_PARTY_NOTICES
    ];
  };

  vendorHash = "sha256-R91IodL/yzGPVeSY5SCYJOcPy83tnui+j0ErFwAW4lg=";
  subPackages = [ "cmd/mediastub" ];
  env.CGO_ENABLED = 0;

  checkPhase = ''
    runHook preCheck
    go test ./...
    runHook postCheck
  '';

  meta = {
    description = "Read-only FUSE filesystem providing metadata-only media views";
    homepage = "https://github.com/qbisi/mediastub";
    license = lib.licenses.mit;
    mainProgram = "mediastub";
    platforms = lib.platforms.linux;
  };
}
