# mediastub

`mediastub` provides two independent ways to expose metadata-only media files
from a local directory or WebDAV collection:

- `mediastub mount` is a process-aware, read-only FUSE projection with stub and
  passthrough views;
- `mediastub sync` creates ordinary sparse files locally and synchronizes
  Jellyfin sidecars back to the remote.

Both modes keep the original media logical size and the container metadata used
by scanners such as Jellyfin; payload bytes in a stub are sparse zero-filled
holes.

It does not import rclone. The boundary between the media logic and an upstream
is only a sized `ReaderAt`, so another process or backend can be added without
changing the probe implementation.

## Build

Go 1.24 or newer is required. FUSE 3 is required only for `mount`; `sync` does
not use FUSE.

```sh
go build -o ./bin/mediastub ./cmd/mediastub
```

With Nix, build or run the default package directly:

```sh
nix build
nix run . -- mount file:///srv/media /tmp/mediastub-mount
```

The flake exports `packages.<system>.mediastub` (also `default`),
`overlays.default`, and `nixosModules.mediastub` (also `default`). To consume
the package from a NixOS flake:

```nix
{
  inputs.mediastub.url = "github:qbisi/mediastub";

  outputs = inputs@{ nixpkgs, mediastub, ... }: {
    nixosConfigurations.example = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      specialArgs = { inherit inputs; };
      modules = [
        mediastub.nixosModules.default
        ({ pkgs, ... }: {
          environment.systemPackages = [ pkgs.mediastub ];
        })
      ];
    };
  };
}
```

The module always adds the package overlay. It can manage mount and sync
services independently:

```nix
{ config, ... }:
{
  services.mediastub = {
    enable = true;
    mounts.h-enc = {
      remote = "http+unix://%2Frun%2Fopenlist%2Fsocket/dav/qbt/H-Enc";
      mountPoint = "/data/H-Enc";
      environmentFile = config.sops.secrets.mediastub-h-enc.path;
      consumers = [ "jellyfin.service" ];
      include = [ "*.mkv" "*.mp4" ];
      options = [
        "--allow-other"
        "--stub-process=ffprobe"
        "--log-level=info"
      ];
    };

    syncs.movies = {
      remote = "https://drive.example/dav/movies";
      localDirectory = "/srv/media/movies";
      environmentFile = config.sops.secrets.mediastub-movies.path;
      consumers = [ "jellyfin.service" ];
      group = "media";
      include = [ "*.mkv" "*.mp4" ];
      pollInterval = 300;
      settleTime = 3;
    };
  };
}
```

Mount services run as the module-created `mediastub:mediastub` system account
by default and do not use `--allow-other`. The module creates the mountpoint,
enables the NixOS FUSE support, and waits until each mount is ready. `consumers`
contains systemd service names that require the mount and are ordered after it;
it has no service-specific defaults. `user` and `group` can be overridden per
mount.

`options` uses the same spelling as the CLI. Including `--allow-other` (or
`--allow-other=true`) also enables `programs.fuse.userAllowOther`; this is
normally required when a consumer runs as a different user.

`include` is shared by mount and sync and renders one `--include` argument. A
mount cannot combine typed `include` with a raw `--include` in `options`.
Sync services run as `mediastub:mediastub` by default, use `Type=notify`, and
allow consumers to start only after the initial reconcile. The module creates
the private state directory, but intentionally does not create or change the
owner/mode of `localDirectory`; create it separately and give the sync user and
Jellyfin access through a shared group such as `media`. A sync-only
configuration does not enable FUSE.

`environmentFile` is read at runtime by systemd. Basic authentication uses:

```text
WEBDAV_USER=alice
WEBDAV_PASSWORD=secret
```

Bearer authentication instead uses exactly:

```text
WEBDAV_TOKEN=secret
```

Basic and Bearer variables are mutually exclusive.

Keep that file outside the Nix store, for example under `/run/secrets` using
sops-nix or agenix. The option is unnecessary for `file://` origins and WebDAV
servers without authentication.

`ffprobe` is used by an optional compatibility test, not at runtime. `ffmpeg`,
`tar` and `gzip` are only needed when regenerating the committed media fixture.

## Command

```text
mediastub mount [options] REMOTE MOUNTPOINT
mediastub sync [options] REMOTE LOCAL_DIRECTORY
```

`REMOTE` supports these forms:

| Form | Origin |
| --- | --- |
| `file:///absolute/path` | Local directory |
| `http://host:port/url-path` | WebDAV over TCP |
| `https://host:port/url-path` | WebDAV over TLS |
| `http+unix://%2Fpath%2Fto%2Fsocket/url-path` | WebDAV over a Unix socket |

The socket path in an `http+unix` authority must be percent encoded. The URL
path after the authority is the WebDAV collection path and is not part of the
socket filename.

### Mount a local directory

```sh
mkdir -p /tmp/mediastub-mount
./bin/mediastub mount \
  file:///srv/media \
  /tmp/mediastub-mount
```

Paths containing spaces or other reserved characters should be URL encoded.
For example, `/srv/My Media` becomes `file:///srv/My%20Media`.

### Mount WebDAV over TCP

```sh
mkdir -p /tmp/mediastub-mount
WEBDAV_USER=alice \
WEBDAV_PASSWORD='secret' \
./bin/mediastub mount \
  http://127.0.0.1:18686/media/ \
  /tmp/mediastub-mount
```

Use an `https://host/url-path` remote for WebDAV over TLS; certificate
verification uses the system trust store.

### Mount WebDAV over a Unix socket

For a WebDAV server listening on `/run/webdav.sock` and exporting `/media/`:

```sh
WEBDAV_USER=alice \
WEBDAV_PASSWORD='secret' \
./bin/mediastub mount \
  'http+unix://%2Frun%2Fwebdav.sock/media/' \
  /tmp/mediastub-mount
```

WebDAV credentials are accepted only through `WEBDAV_USER` plus
`WEBDAV_PASSWORD`, or `WEBDAV_TOKEN`; credentials embedded in a remote URL are
rejected. Partial or mixed authentication is rejected before accessing the
remote. Authentication headers and cookies are removed when a request is
redirected to a different scheme or authority.
The server must support `PROPFIND` and byte-range `GET`. Plain `http://` should
only be used on a trusted network or through a protected local tunnel.
For `http+unix`, WebDAV requests use the Unix socket while absolute redirects
to signed HTTP or HTTPS object URLs use the normal network transport. Query
parameters from signed URLs are redacted from transport error logs.

Unmount with `Ctrl-C` or:

```sh
fusermount3 -u /tmp/mediastub-mount
```

Mount options may appear before or after `REMOTE MOUNTPOINT`, for example:

```sh
mediastub mount REMOTE MOUNTPOINT --allow-other
```

## Sidecar synchronization

`sync` is deliberately not a general two-way synchronization tool. Media files
are authoritative on the remote and flow only remote to local. Sidecars are
authoritative locally: local additions and changes are uploaded, while remote
sidecars only fill a missing local file.

```sh
mediastub sync \
  --state-dir /var/lib/mediastub-movies \
  --include '*.mkv,*.mp4' \
  --poll-interval 5m \
  --settle-time 3s \
  https://drive.example/dav/movies \
  /srv/media/movies
```

Use `--once` for one complete remote scan, local scan and reconcile. The state
directory is mandatory, must be absolute, and is locked so two processes cannot
use it simultaneously. Its `Remote` and `LocalRoot` identity must continue to
match subsequent invocations.

For every included remote Matroska or MP4 object, sync probes the remote using a
fixed 16 MiB / 128 request / 256 KiB window budget and atomically creates a
read-only ordinary sparse file. An existing media path is replaced only when it
is already recorded as a managed stub. State loss therefore fails closed rather
than overwriting a possible real local media file.

Recognized sidecars must share a directory and stem with a managed media file:

- exact `<stem>.nfo`;
- common Jellyfin image forms such as `<stem>.jpg`, `-poster`, `-cover`,
  `-fanartN`, `-backdropN`, `-thumb`, `-logo`, `-art` and `-disc` using JPG,
  JPEG, PNG or WebP;
- SRT, ASS, SSA, VTT or SUB subtitles with zero, one or two qualifiers, such as
  `movie.zh.forced.srt`.

The longest matching media stem wins. An equal-length ambiguity is logged and
skipped. Other images and subtitles are not treated as sidecars.

First synchronization is always `prefer-local`: differing local content
overwrites the remote. Uploads use a direct PUT to the final path, followed by
a complete SHA-256 read-back with bounded retries. This intentionally does not
depend on `If-Match`, `If-None-Match`, `MOVE Overwrite:F`, or a remote temporary
file. A duplicate remote path is logged with all available fingerprints and is
skipped without selecting an object.

Deleting a local sidecar creates a persistent tombstone. It does not delete the
remote object and the old remote copy is not downloaded again. Recreating the
local path clears the tombstone and uploads it. Remote media and sidecar deletes
are not propagated locally in v1. Every poll also performs a complete local
scan, so missed filesystem notifications recover automatically.

## Probe policy

The default include patterns are:

```text
*.mkv,*.mka,*.mks,*.webm,*.mp4,*.m4v,*.mov
```

Use `--include` to narrow them. Patterns follow Go's `path.Match`; patterns
containing `/` apply to the complete relative path, while other patterns apply
to the basename. For example:

```sh
mediastub mount --include '*.mkv,Anime/*.mp4' REMOTE MOUNTPOINT
```

## Process selection

By default, UID and GID matching are disabled, and only a process whose
`/proc/PID/comm` is `ffprobe` receives a stub view. Other processes read the
original upstream bytes, even for a media file matched by `--include`:

```sh
mediastub mount --stub-process 'ffprobe,jellyfin-probe*' REMOTE MOUNTPOINT
```

UID, effective GID and `comm` rules are combined with OR semantics. This gives
the stub view to UID 1000, effective GID 991, or either named process:

```sh
mediastub mount \
  --stub-uid '1000' \
  --stub-gid '991' \
  --stub-process 'ffprobe,jellyfin-probe*' \
  REMOTE MOUNTPOINT
```

`--stub-uid` and `--stub-gid` accept comma-separated unsigned numeric IDs. GID
matching uses the effective GID carried by the FUSE request; supplementary
group membership is not available in the request and is therefore not
matched. UID/GID rules are checked first and do not require a `/proc` lookup.
To select only by UID/GID, explicitly disable the default comm rule:

```sh
mediastub mount --stub-process '' --stub-uid '1000,1001' REMOTE MOUNTPOINT
```

At least one of the three selectors must remain configured. `--stub-process`
is a comma-separated list of `path.Match` patterns. Use `*` to restore the
all-process stub behavior. If the caller identity is unavailable, or a required
`comm` lookup fails after UID/GID did not match, mediastub safely falls back to
the original upstream view. Linux limits `comm` to 15 bytes, so patterns must
match the value actually exposed by `/proc/PID/comm`, not necessarily the full
executable filename.

Stub handles use FUSE direct I/O so their zero-filled pages never contaminate
the shared kernel page cache used by passthrough readers. Selection is fixed
when a file is opened; passing an already-open file descriptor to another
process does not change its view. Process matching is routing policy, not a
security boundary, because a process can change its own `comm` value.

The current filesystem remains globally read-only. A non-matching process gets
the original bytes but does not yet gain write access.

## Logging

Every access log records the caller PID, effective UID/GID, the available
`/proc/PID/comm`, the relative path, whether `--include` matched, and the
selected view. The `process` field is empty when UID/GID matching short-circuits
the comm lookup. The default `--log-level info` records opens only for paths
matched by `--include`:

```text
mediastub: 2026/07/14 01:09:34.493535 access pid=495448 uid=1000 gid=100 process="ffprobe" path="movie.mkv" include=true route=stub
```

When a probe finishes, its completion log includes the elapsed probe time from
probe start until the stub plan is ready:

```text
mediastub: 2026/07/14 01:09:34.493510 stub ready path="movie.mkv" format=matroska probe_bytes=262144 requests=1 probe_time=1.284732s
```

Fallback probes report the same field on their `probe skipped` line.

Increase logging one level to record opens of every path, including files that
are passed through unchanged:

```sh
mediastub mount --log-level verbose REMOTE MOUNTPOINT
```

```text
mediastub: 2026/07/14 01:09:35.012345 access pid=495449 uid=1000 gid=100 process="jellyfin" path="poster.jpg" include=false route=passthrough
```

`--log-level debug` includes the same complete access log and enables the raw
go-fuse request/response trace (`LOOKUP`, `OPEN`, `READ`, `RELEASE`, and so on).
It is intentionally very verbose and is intended for short diagnostics. Mount
lifecycle messages, probe results and real backend errors are emitted at every
level. Expected negative lookups such as probes for `.git` are returned as
`ENOENT` without a high-level error line; debug mode still shows the underlying
FUSE request and response.

Each successful probe is cached by path, size, modification time and ETag.
Concurrent opens share one probe. The default hard limits are 16 MiB and 128
upstream reads per object, with 256 KiB read coalescing. Relevant flags are:

```text
--probe-max-read
--probe-max-requests
--probe-window-size
--plan-cache-entries
--on-probe-error passthrough|fail
--stub-process
--stub-uid
--stub-gid
--log-level info|verbose|debug
```

`passthrough` is the default: unrecognized, unsupported or malformed objects
remain readable as their original bytes. `fail` instead makes an eligible file
unopenable when its media probe fails. Files not selected by `--include` always
pass through unchanged.

The FUSE filesystem is unconditionally mounted read-only. Only `sync` writes
recognized sidecars, and it never uploads local media or issues remote DELETE,
MOVE or COPY operations.

## Tests

```sh
CGO_ENABLED=0 go test ./...
go test ./core -run 'TestMediaRangeSuite|TestMediaRangeSuiteFFprobeStubs' -v
nix flake check
```

The flake check builds the package and `checks.<system>.module-eval`, which
uses `nixosConfigurations.check-<system>` to evaluate sample mount and sync
units. `nixosConfigurations.sync-only-<system>` also verifies that sync alone
does not enable FUSE. Module values can be inspected
directly, for example:

```sh
nix eval \
  .#nixosConfigurations.check-aarch64-linux.config.systemd.services.mediastub-check.serviceConfig.User
```

The tests extract the committed `testdata/media-range-suite.tar.gz`, so normal
test runs do not require ffmpeg. Regenerate both a review directory and the
archive with:

```sh
./testdata/generate-media-range-suite.sh \
  /tmp/mediastub-suite \
  ./testdata/media-range-suite.tar.gz
```

Archive entry ordering, timestamps and ownership are normalized by the script.
The encoded media may still vary with the ffmpeg version, so regenerated
archives should be reviewed as intentional fixture updates.

## Package boundaries

- `core`: container detection and immutable sparse read plans; standard library
  only, and unaware of filesystems or HTTP.
- `origin`: the namespace and random-read contract, plus the optional sidecar
  PUT extension, with `local` and `webdav` implementations.
- `pathfilter`: include parsing and `path.Match` behavior shared by both modes.
- `mountfs`: policy, plan caching and the read-only go-fuse projection.
- `syncer`: transactional scans, sparse materialization, sidecar classification,
  tombstones, state, file watching and serialized reconciliation.
- `internal/sdnotify`: minimal systemd readiness notification.
- `cmd/mediastub`: CLI wiring and lifecycle only.

The project is distributed under the MIT license; see [COPYING](COPYING).
Runtime dependency notices required for binary redistribution are in
[THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES).
