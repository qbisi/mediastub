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
        system:
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
                mounts.check = {
                  remote = "http+unix://%2Frun%2Fopenlist%2Fsocket/dav/media";
                  mountPoint = "/run/mediastub-check";
                  consumers = [ "media-server.service" ];
                  options = [
                    "--allow-other"
                    "--include=*.mkv"
                  ];
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
          pkgs = evaluated.pkgs;
          unit = evaluated.config.systemd.units."mediastub-check.service".unit;
        in
        {
          module-eval = pkgs.runCommand "mediastub-module-eval" { } ''
            unit=${unit}/mediastub-check.service
            test -f "$unit"
            ${pkgs.gnugrep}/bin/grep -Fq "User=mediastub" "$unit"
            ${pkgs.gnugrep}/bin/grep -Fq -- "--allow-other" "$unit"
            ${pkgs.gnugrep}/bin/grep -Fq "http+unix://%%2Frun%%2Fopenlist%%2Fsocket/dav/media" "$unit"
            ${pkgs.gnugrep}/bin/grep -Fq "Before=media-server.service" "$unit"
            touch "$out"
          '';
        }
      );

      nixosConfigurations = builtins.listToAttrs (
        map (system: {
          name = "check-${system}";
          value = mkCheckConfiguration system;
        }) supportedSystems
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
