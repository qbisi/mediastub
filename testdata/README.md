# Media range fixture

`media-range-suite.tar.gz` is the committed input fixture used by the core
probe and ffprobe compatibility tests. It contains the `media/` directory with
normal, tail-metadata, fast-start, large-void and truncated container cases.

The archive is generated entirely from synthetic ffmpeg sources. To regenerate
it after intentionally changing the fixture definition:

```sh
./testdata/generate-media-range-suite.sh \
  /tmp/mediastub-suite \
  ./testdata/media-range-suite.tar.gz
```

The script normalizes tar ordering, timestamps, ownership and the gzip header.
Media payload bytes can still change across ffmpeg versions; inspect probe
statistics and ffprobe assertions before accepting a regenerated archive.
