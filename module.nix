{
  config,
  inputs,
  lib,
  pkgs,
  self,
  ...
}:
let
  cfg = config.services.mediastub;
  nonEmptyString = lib.types.strMatching ".+";

  mountSubmodule =
    { name, ... }:
    {
      options = {
        remote = lib.mkOption {
          type = nonEmptyString;
          description = "Local or WebDAV remote passed to mediastub mount.";
          example = "http://127.0.0.1:8080/dav/media";
        };

        mountPoint = lib.mkOption {
          type = lib.types.strMatching "/.*";
          description = "Absolute path where this remote is mounted.";
          example = "/data/media";
        };

        user = lib.mkOption {
          type = nonEmptyString;
          default = "mediastub";
          description = "User that owns and runs the FUSE mount.";
        };

        group = lib.mkOption {
          type = nonEmptyString;
          default = "mediastub";
          description = "Group that owns and runs the FUSE mount.";
        };

        environmentFile = lib.mkOption {
          type = lib.types.nullOr (lib.types.strMatching "/.*");
          default = null;
          description = ''
            Runtime systemd environment file containing WEBDAV_USER and
            WEBDAV_PASSWORD. Its contents are not copied to the Nix store.
          '';
        };

        consumers = lib.mkOption {
          type = lib.types.listOf nonEmptyString;
          default = [ ];
          description = ''
            Systemd services that require this mount and are ordered after it.
            Names may be written with or without the .service suffix.
          '';
          example = [ "media-server.service" ];
        };

        options = lib.mkOption {
          type = lib.types.listOf nonEmptyString;
          default = [ ];
          description = "mediastub mount options placed before REMOTE and MOUNTPOINT.";
          example = [
            "--allow-other"
            "--include=*.mkv,*.mp4"
            "--stub-process=ffprobe"
            "--log-level=verbose"
          ];
        };
      };
    };

  escapedName =
    name:
    let
      sanitized = lib.strings.sanitizeDerivationName name;
    in
    if sanitized == name then
      sanitized
    else
      "${sanitized}-${builtins.substring 0 8 (builtins.hashString "sha256" name)}";
  serviceName = name: "mediastub-${escapedName name}";
  mounts = builtins.attrValues cfg.mounts;
  consumerUnit = name: "${lib.removeSuffix ".service" name}.service";
  usesAllowOther =
    mount:
    lib.any (option: option == "--allow-other" || lib.hasPrefix "--allow-other=" option) mount.options;

  mkWaitScript =
    name: mount:
    pkgs.writeShellScript "wait-for-${serviceName name}" ''
      for _ in {1..100}; do
        if ${lib.getExe' pkgs.util-linux "mountpoint"} -q ${lib.escapeShellArg mount.mountPoint}; then
          exit 0
        fi
        ${lib.getExe' pkgs.coreutils "sleep"} 0.1
      done
      echo "mediastub mount did not become ready: ${mount.mountPoint}" >&2
      exit 1
    '';

  mkService =
    name: mount:
    lib.nameValuePair (serviceName name) {
      description = "mediastub mount ${name}";
      wantedBy = [ "multi-user.target" ];
      requiredBy = map consumerUnit mount.consumers;
      before = map consumerUnit mount.consumers;
      wants = lib.optionals (
        lib.hasPrefix "http://" mount.remote || lib.hasPrefix "https://" mount.remote
      ) [ "network-online.target" ];
      after = lib.optionals (
        lib.hasPrefix "http://" mount.remote || lib.hasPrefix "https://" mount.remote
      ) [ "network-online.target" ];

      path = [ (builtins.dirOf config.security.wrapperDir) ];

      serviceConfig = {
        Type = "simple";
        User = mount.user;
        Group = mount.group;
        UMask = "0027";
        EnvironmentFile = lib.optional (mount.environmentFile != null) mount.environmentFile;
        ExecStart = lib.escapeShellArgs (
          [
            (lib.getExe cfg.package)
            "mount"
          ]
          ++ mount.options
          ++ [
            mount.remote
            mount.mountPoint
          ]
        );
        ExecStartPost = mkWaitScript name mount;
        ExecStopPost = "-fusermount3 -uz ${lib.escapeShellArg mount.mountPoint}";
        Restart = "on-failure";
        RestartSec = "5s";
        KillSignal = "SIGTERM";
        TimeoutStopSec = "30s";
      };
    };
in
{
  options.services.mediastub = {
    enable = lib.mkEnableOption "managed mediastub FUSE mounts";
    package = lib.mkPackageOption pkgs "mediastub" { };
    mounts = lib.mkOption {
      type = lib.types.attrsOf (lib.types.submodule mountSubmodule);
      default = { };
      description = "Named mediastub mounts, each managed by its own systemd service.";
    };
  };

  config = lib.mkMerge [
    {
      nixpkgs.overlays = [ inputs.mediastub.overlays.default or self.overlays.default ];
    }

    (lib.mkIf cfg.enable {
      programs.fuse = {
        enable = true;
        userAllowOther = lib.any usesAllowOther mounts;
      };

      users.groups.mediastub = { };
      users.users.mediastub = {
        isSystemUser = true;
        group = "mediastub";
        description = "mediastub mount service";
      };

      systemd.tmpfiles.settings."10-mediastub" = lib.mapAttrs' (
        _name: mount:
        lib.nameValuePair mount.mountPoint {
          d = {
            mode = "0750";
            user = mount.user;
            group = mount.group;
          };
        }
      ) cfg.mounts;

      systemd.services = lib.mapAttrs' mkService cfg.mounts;
    })
  ];
}
