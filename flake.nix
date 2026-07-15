{
  description = "Metadata-only media views over local files and WebDAV";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    inputs@{ self, nixpkgs }:
    let
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
      mkCheckConfiguration =
        system: withMount:
        nixpkgs.lib.nixosSystem {
          inherit system;
          modules = [
            self.nixosModules.default
            {
              system.stateVersion = "26.11";
              fileSystems."/" = {
                device = "none";
                fsType = "tmpfs";
              };
              boot.loader.grub.devices = [ "nodev" ];
              services.mediastub = {
                enable = true;
                mounts = nixpkgs.lib.optionalAttrs withMount {
                  check = {
                    remote = "http+unix://%2Frun%2Fopenlist%2Fsocket/dav/media";
                    mountPoint = "/run/mediastub-check";
                    consumers = [ "media-server.service" ];
                    include = [ "*.mkv" ];
                    options = [ "--allow-other" ];
                  };
                };
                syncs.check = {
                  remote = "https://example.invalid/dav/media";
                  localDirectory = "/srv/media";
                  environmentFile = "/run/secrets/mediastub";
                  consumers = [ "media-server.service" ];
                  include = [
                    "*.mkv"
                    "*.mp4"
                  ];
                  pollInterval = 300;
                  settleTime = 3;
                };
              };
            }
          ];
        };
    in
    {
      overlays.default = final: _prev: {
        mediastub = final.callPackage ./package.nix { };
      };

      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs {
            inherit system;
            overlays = [ self.overlays.default ];
          };
        in
        {
          inherit (pkgs) mediastub;
          default = pkgs.mediastub;
        }
      );

      checks = forAllSystems (
        system:
        let
          evaluated = self.nixosConfigurations."check-${system}";
          syncOnly = self.nixosConfigurations."sync-only-${system}";
          pkgs = evaluated.pkgs;
          unit = evaluated.config.systemd.units."mediastub-check.service".unit;
          syncUnit = evaluated.config.systemd.units."mediastub-sync-check.service".unit;
        in
        {
          module-eval =
            assert !syncOnly.config.programs.fuse.enable;
            pkgs.runCommand "mediastub-module-eval" { } ''
              unit=${unit}/mediastub-check.service
              sync_unit=${syncUnit}/mediastub-sync-check.service
              test -f "$unit"
              test -f "$sync_unit"
              ${pkgs.gnugrep}/bin/grep -Fq "User=mediastub" "$unit"
              ${pkgs.gnugrep}/bin/grep -Fq -- "--allow-other" "$unit"
              ${pkgs.gnugrep}/bin/grep -Fq -- "--include=*.mkv" "$unit"
              ${pkgs.gnugrep}/bin/grep -Fq "http+unix://%%2Frun%%2Fopenlist%%2Fsocket/dav/media" "$unit"
              ${pkgs.gnugrep}/bin/grep -Fq "Before=media-server.service" "$unit"
              ${pkgs.gnugrep}/bin/grep -Fq "Type=notify" "$sync_unit"
              ${pkgs.gnugrep}/bin/grep -Fq "StateDirectory=mediastub-sync-check" "$sync_unit"
              ${pkgs.gnugrep}/bin/grep -Fq "UMask=0002" "$sync_unit"
              ${pkgs.gnugrep}/bin/grep -Fq -- "--include=*.mkv,*.mp4" "$sync_unit"
              ${pkgs.gnugrep}/bin/grep -Fq -- "--poll-interval=300s" "$sync_unit"
              ${pkgs.gnugrep}/bin/grep -Fq -- "--settle-time=3s" "$sync_unit"
              ${pkgs.gnugrep}/bin/grep -Fq "Before=media-server.service" "$sync_unit"
              touch "$out"
            '';
        }
      );

      nixosConfigurations = builtins.listToAttrs (
        builtins.concatMap (system: [
          {
            name = "check-${system}";
            value = mkCheckConfiguration system true;
          }
          {
            name = "sync-only-${system}";
            value = mkCheckConfiguration system false;
          }
        ]) supportedSystems
      );

      nixosModules = rec {
        mediastub = {
          imports = [ ./module.nix ];
          _module.args = { inherit inputs self; };
        };
        default = mediastub;
      };
    };
}
