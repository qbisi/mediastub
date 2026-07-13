package core

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fileSource struct {
	*os.File
	size int64
}

func openFileSource(t *testing.T, path string) *fileSource {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.Close()) })
	info, err := f.Stat()
	require.NoError(t, err)
	return &fileSource{File: f, size: info.Size()}
}

func (s *fileSource) Size() int64 { return s.size }

func TestPlanReadAt(t *testing.T) {
	plan, err := NewPlan(12, []Extent{{Offset: 1, Data: []byte("abc")}, {Offset: 8, Data: []byte("xy")}})
	require.NoError(t, err)
	buf := make([]byte, 12)
	n, err := plan.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, 12, n)
	assert.Equal(t, []byte{0, 'a', 'b', 'c', 0, 0, 0, 0, 'x', 'y', 0, 0}, buf)
}

func TestMediaRangeSuite(t *testing.T) {
	root := extractMediaRangeSuite(t)
	tests := []struct {
		name      string
		format    Format
		wantError bool
		maxRead   int64
		requests  int
	}{
		{name: "01_mkv_normal_front.mkv", format: FormatMatroska, maxRead: 256 << 10, requests: 1},
		{name: "02_mkv_large_void_before_cluster.mkv", format: FormatMatroska, maxRead: 256 << 10, requests: 1},
		{name: "03_mp4_moov_at_end.mp4", format: FormatMP4, maxRead: 512 << 10, requests: 2},
		{name: "04_mp4_faststart.mp4", format: FormatMP4, maxRead: 256 << 10, requests: 1},
		{name: "10_truncated_mp4_no_moov.mp4", wantError: true},
		{name: "11_truncated_mkv_header_and_packets.mkv", format: FormatMatroska, maxRead: 256 << 10, requests: 1},
		{name: "12_mkv_with_bin_extension.bin", format: FormatMatroska, maxRead: 256 << 10, requests: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := openFileSource(t, filepath.Join(root, "media", test.name))
			result, err := Probe(source, DefaultBudget)
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.format, result.Format)
			assert.LessOrEqual(t, result.Stats.Bytes, test.maxRead)
			assert.Equal(t, test.requests, result.Stats.Requests)
			assert.Equal(t, source.Size(), result.Plan.Size())

			stubResult, err := Probe(result.Plan, DefaultBudget)
			require.NoError(t, err)
			assert.Equal(t, result.Format, stubResult.Format)
		})
	}
}

func TestMediaRangeSuiteFFprobeStubs(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed; skipping stub compatibility test")
	}
	root := extractMediaRangeSuite(t)
	for _, name := range []string{"01_mkv_normal_front.mkv", "03_mp4_moov_at_end.mp4"} {
		t.Run(name, func(t *testing.T) {
			source := openFileSource(t, filepath.Join(root, "media", name))
			result, err := Probe(source, DefaultBudget)
			require.NoError(t, err)

			stubPath := filepath.Join(t.TempDir(), name)
			stub, err := os.Create(stubPath)
			require.NoError(t, err)
			_, copyErr := io.Copy(stub, io.NewSectionReader(result.Plan, 0, result.Plan.Size()))
			closeErr := stub.Close()
			require.NoError(t, copyErr)
			require.NoError(t, closeErr)

			cmd := exec.Command("ffprobe", "-v", "error", "-show_format", "-show_streams", "-of", "json", stubPath)
			output, err := cmd.CombinedOutput()
			require.NoErrorf(t, err, "ffprobe rejected generated stub:\n%s", output)
			probeJSON := string(output)
			assert.Contains(t, probeJSON, `"codec_name": "h264"`)
			assert.Contains(t, probeJSON, `"codec_name": "aac"`)
			assert.Contains(t, probeJSON, `"width": 320`)
			assert.Contains(t, probeJSON, `"height": 180`)
			assert.Contains(t, probeJSON, `"display_aspect_ratio": "16:9"`)
			assert.Contains(t, probeJSON, `"avg_frame_rate": "24000/1001"`)
			assert.Contains(t, probeJSON, `"sample_rate": "44100"`)
			assert.Contains(t, probeJSON, `"channels": 1`)
			assert.Contains(t, probeJSON, `"language": "jpn"`)
			assert.Contains(t, probeJSON, `"default": 1`)
			assert.Contains(t, probeJSON, `"forced": 0`)
			assert.Contains(t, probeJSON, `"duration": "6.`)
		})
	}
}

var (
	mediaSuiteOnce sync.Once
	mediaSuiteRoot string
	mediaSuiteErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if mediaSuiteRoot != "" {
		_ = os.RemoveAll(mediaSuiteRoot)
	}
	os.Exit(code)
}

func extractMediaRangeSuite(t *testing.T) string {
	t.Helper()
	mediaSuiteOnce.Do(func() {
		mediaSuiteRoot, mediaSuiteErr = os.MkdirTemp("", "mediastub-media-range-suite-")
		if mediaSuiteErr != nil {
			return
		}
		archive := filepath.Join("..", "testdata", "media-range-suite.tar.gz")
		mediaSuiteErr = extractTarGzip(archive, mediaSuiteRoot)
	})
	require.NoError(t, mediaSuiteErr)
	return mediaSuiteRoot
}

func extractTarGzip(archive, destination string) error {
	file, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("open media range archive: %w", err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open media range gzip stream: %w", err)
	}
	defer gz.Close()

	const maxExtractedBytes = 128 << 20
	var extracted int64
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read media range tar: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe media range archive path %q", header.Name)
		}
		target := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			extracted += header.Size
			if header.Size < 0 || extracted > maxExtractedBytes {
				return fmt.Errorf("media range archive exceeds %d bytes", maxExtractedBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(output, reader)
			closeErr := output.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsupported media range archive entry %q (type %d)", header.Name, header.Typeflag)
		}
	}
}
