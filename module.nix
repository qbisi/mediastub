{
  config,
  inputs,
  lib,
  pkgs,
  self,
  utils,
  ...
}:
let
  cfg = config.services.mediastub;
  nonEmptyString = lib.types.strMatching ".+";

  includeOption = lib.mkOption {
    type = lib.types.nullOr (lib.types.listOf nonEmptyString);
    default = null;
    description = ''
      Media path.Match patterns. A null value uses the mediastub CLI default.
      Patterns containing "/" match the complete relative path; other
      patterns match the basename.
    '';
    example = [
      "*.mkv"
      "*.mp4"
      "Anime/*.webm"
    ];
  };

  commonOptions = {
    remote = lib.mkOption {
      type = nonEmptyString;
      description = "Local or WebDAV remote passed to mediastub.";
      example = "http://127.0.0.1:8080/dav/media";
    };
    user = lib.mkOption {
      type = nonEmptyString;
      default = "mediastub";
      description = "User that runs the mediastub service.";
    };
    group = lib.mkOption {
      type = nonEmptyString;
      default = "mediastub";
      description = "Primary group of the mediastub service.";
    };
    environmentFile = lib.mkOption {
      type = lib.types.nullOr (lib.types.strMatching "/.*");
      default = null;
      description = ''
        Runtime systemd environment file containing either WEBDAV_USER and
        WEBDAV_PASSWORD, or WEBDAV_TOKEN. Its contents are not copied to the
        Nix store.
      '';
    };
    consumers = lib.mkOption {
      type = lib.types.listOf nonEmptyString;
      default = [ ];
      description = "Systemd services that require this service and are ordered after it.";
      example = [ "media-server.service" ];
    };
    include = includeOption;
  };

  mountSubmodule = {
    options = commonOptions // {
      mountPoint = lib.mkOption {
        type = lib.types.strMatching "/.*";
        description = "Absolute path where this remote is mounted.";
        example = "/data/media";
      };
      options = lib.mkOption {
        type = lib.types.listOf nonEmptyString;
        default = [ ];
        description = "Additional mediastub mount options placed before REMOTE and MOUNTPOINT.";
        example = [
          "--allow-other"
          "--stub-process=ffprobe"
          "--log-level=verbose"
        ];
      };
    };
  };

  syncSubmodule = {
    options = commonOptions // {
      localDirectory = lib.mkOption {
        type = lib.types.strMatching "/.*";
        description = "Existing local directory containing sparse stubs and sidecars.";
        example = "/srv/media/movies";
      };
      pollInterval = lib.mkOption {
        type = lib.types.ints.positive;
        default = 300;
        description = "Complete remote scan interval in seconds.";
      };
      settleTime = lib.mkOption {
        type = lib.types.ints.positive;
        default = 3;
        description = "Seconds a local sidecar must remain unchanged before upload.";
      };
      logLevel = lib.mkOption {
        type = lib.types.enum [
          "info"
          "verbose"
          "debug"
        ];
        default = "info";
        description = "Synchronization log detail.";
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
  mountServiceName = name: "mediastub-${escapedName name}";
  syncServiceName = name: "mediastub-sync-${escapedName name}";
  syncStateName = name: "mediastub-sync-${escapedName name}";
  mounts = builtins.attrValues cfg.mounts;
  syncs = builtins.attrValues cfg.syncs;
  consumerUnit = name: "${lib.removeSuffix ".service" name}.service";
  hasRawInclude =
    mount: lib.any (option: option == "--include" || lib.hasPrefix "--include=" option) mount.options;
  includeArgs = value: lib.optional (value != null) "--include=${lib.concatStringsSep "," value}";
  usesAllowOther =
    mount:
    lib.any (option: option == "--allow-other" || lib.hasPrefix "--allow-other=" option) mount.options;
  needsNetwork = remote: lib.hasPrefix "http://" remote || lib.hasPrefix "https://" remote;

  mkWaitScript =
    name: mount:
    pkgs.writeShellScript "wait-for-${mountServiceName name}" ''
      for _ in {1..100}; do
        if ${lib.getExe' pkgs.util-linux "mountpoint"} -q ${lib.escapeShellArg mount.mountPoint}; then
          exit 0
        fi
        ${lib.getExe' pkgs.coreutils "sleep"} 0.1
      done
      echo "mediastub mount did not become ready: ${mount.mountPoint}" >&2
      exit 1
    '';

  mkMountService =
    name: mount:
    lib.nameValuePair (mountServiceName name) {
      description = "mediastub mount ${name}";
      wantedBy = [ "multi-user.target" ];
      requiredBy = map consumerUnit mount.consumers;
      before = map consumerUnit mount.consumers;
      wants = lib.optionals (needsNetwork mount.remote) [ "network-online.target" ];
      after = lib.optionals (needsNetwork mount.remote) [ "network-online.target" ];
      path = [ (builtins.dirOf config.security.wrapperDir) ];
      serviceConfig = {
        Type = "simple";
        User = mount.user;
        Group = mount.group;
        UMask = "0027";
        EnvironmentFile = lib.optional (mount.environmentFile != null) mount.environmentFile;
        ExecStart = utils.escapeSystemdExecArgs (
          [
            (lib.getExe cfg.package)
            "mount"
          ]
          ++ includeArgs mount.include
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

  mkSyncExecStart =
    name: sync:
    lib.replaceStrings [ "@MEDIASTUB_STATE_DIRECTORY@" ] [ "%S/${syncStateName name}" ] (
      utils.escapeSystemdExecArgs (
        [
          (lib.getExe cfg.package)
          "sync"
          "--state-dir=@MEDIASTUB_STATE_DIRECTORY@"
          "--poll-interval=${toString sync.pollInterval}s"
          "--settle-time=${toString sync.settleTime}s"
          "--log-level=${sync.logLevel}"
        ]
        ++ includeArgs sync.include
        ++ [
          sync.remote
          sync.localDirectory
        ]
      )
    );

  mkSyncService =
    name: sync:
    lib.nameValuePair (syncServiceName name) {
      description = "mediastub sidecar synchronization ${name}";
      wantedBy = [ "multi-user.target" ];
      requiredBy = map consumerUnit sync.consumers;
      before = map consumerUnit sync.consumers;
      wants = lib.optionals (needsNetwork sync.remote) [ "network-online.target" ];
      after = lib.optionals (needsNetwork sync.remote) [ "network-online.target" ];
      serviceConfig = {
        Type = "notify";
        NotifyAccess = "main";
        User = sync.user;
        Group = sync.group;
        UMask = "0002";
        EnvironmentFile = lib.optional (sync.environmentFile != null) sync.environmentFile;
        StateDirectory = syncStateName name;
        StateDirectoryMode = "0750";
        ExecStart = mkSyncExecStart name sync;
        Restart = "on-failure";
        RestartSec = "5s";
        KillSignal = "SIGTERM";
        TimeoutStartSec = "15min";
        TimeoutStopSec = "2min";
        ReadWritePaths = [ sync.localDirectory ];
      };
    };

  mountEscapedNames = map escapedName (builtins.attrNames cfg.mounts);
  syncEscapedNames = map escapedName (builtins.attrNames cfg.syncs);
  syncDirectories = map (sync: sync.localDirectory) syncs;
  mountPoints = map (mount: mount.mountPoint) mounts;
in
{
  options.services.mediastub = {
    enable = lib.mkEnableOption "managed mediastub mounts and sidecar synchronization";
    package = lib.mkPackageOption pkgs "mediastub" { };
    mounts = lib.mkOption {
      type = lib.types.attrsOf (lib.types.submodule mountSubmodule);
      default = { };
      description = "Named read-only FUSE mounts.";
    };
    syncs = lib.mkOption {
      type = lib.types.attrsOf (lib.types.submodule syncSubmodule);
      default = { };
      description = "Named sparse media and sidecar synchronization services.";
    };
  };

  config = lib.mkMerge [
    { nixpkgs.overlays = [ inputs.mediastub.overlays.default or self.overlays.default ]; }

    (lib.mkIf cfg.enable {
      assertions = [
        {
          assertion = lib.length mountEscapedNames == lib.length (lib.unique mountEscapedNames);
          message = "services.mediastub.mounts names collide after systemd escaping";
        }
        {
          assertion = lib.length syncEscapedNames == lib.length (lib.unique syncEscapedNames);
          message = "services.mediastub.syncs names collide after systemd escaping";
        }
        {
          assertion = lib.length syncDirectories == lib.length (lib.unique syncDirectories);
          message = "services.mediastub.syncs must use distinct localDirectory values";
        }
        {
          assertion = lib.all (directory: !(builtins.elem directory mountPoints)) syncDirectories;
          message = "a mediastub sync localDirectory must not equal a managed mountPoint";
        }
      ]
      ++ map (mount: {
        assertion = mount.include == null || !hasRawInclude mount;
        message = "a mediastub mount cannot set both include and a raw --include option";
      }) mounts;

      users.groups.mediastub = { };
      users.users.mediastub = {
        isSystemUser = true;
        group = "mediastub";
        description = "mediastub service";
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

      systemd.services =
        (lib.mapAttrs' mkMountService cfg.mounts) // (lib.mapAttrs' mkSyncService cfg.syncs);
    })

    (lib.mkIf (cfg.enable && cfg.mounts != { }) {
      programs.fuse = {
        enable = true;
        userAllowOther = lib.any usesAllowOther mounts;
      };
    })
  ];
}
