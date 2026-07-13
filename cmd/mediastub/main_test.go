package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"testing"
)

func TestParseMount(t *testing.T) {
	opts, remote, mountpoint, err := parseMount([]string{
		"--probe-max-read", "8MiB",
		"--include", "*.mkv,Anime/*.mp4",
		"file:///srv/media", "/data/media",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if remote != "file:///srv/media" || mountpoint != "/data/media" {
		t.Fatalf("remote=%q mountpoint=%q", remote, mountpoint)
	}
	if opts.maxRead != 8<<20 || opts.include != "*.mkv,Anime/*.mp4" {
		t.Fatalf("unexpected options: %+v", opts)
	}
	if opts.stubProcess != "ffprobe" {
		t.Fatalf("default stub process = %q, want ffprobe", opts.stubProcess)
	}
	if opts.stubUID != "" || opts.stubGID != "" {
		t.Fatalf("default stub IDs = uid %q gid %q, want disabled", opts.stubUID, opts.stubGID)
	}
	if opts.logLevel != "info" {
		t.Fatalf("default log level = %q, want info", opts.logLevel)
	}
}

func TestParseMountUIDGIDSelectors(t *testing.T) {
	opts, _, _, err := parseMount([]string{
		"--stub-process", "", "--stub-uid", "1000, 1001", "--stub-gid=100,991",
		"file:///srv/media", "/data/media",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.stubProcess != "" || opts.stubUID != "1000, 1001" || opts.stubGID != "100,991" {
		t.Fatalf("unexpected selectors: %+v", opts)
	}
	uids, err := numericIDs(opts.stubUID, "--stub-uid")
	if err != nil || len(uids) != 2 || uids[0] != 1000 || uids[1] != 1001 {
		t.Fatalf("UIDs = %v, %v", uids, err)
	}
}

func TestParseMountAcceptsOptionsAfterPositionals(t *testing.T) {
	opts, remote, mountpoint, err := parseMount([]string{
		"http+unix://%2Frun%2Fto%2Fsocket/dav", "/data",
		"--allow-other", "--log-level", "verbose", "--probe-max-read=8MiB",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if remote != "http+unix://%2Frun%2Fto%2Fsocket/dav" || mountpoint != "/data" {
		t.Fatalf("remote=%q mountpoint=%q", remote, mountpoint)
	}
	if !opts.allowOther || opts.logLevel != "verbose" || opts.maxRead != 8<<20 {
		t.Fatalf("unexpected options: %+v", opts)
	}
}

func TestParseMountHonorsEndOfOptions(t *testing.T) {
	_, remote, mountpoint, err := parseMount([]string{"--", "file:///srv/-media", "-mountpoint"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if remote != "file:///srv/-media" || mountpoint != "-mountpoint" {
		t.Fatalf("remote=%q mountpoint=%q", remote, mountpoint)
	}
}

func TestParseMountHelp(t *testing.T) {
	var output bytes.Buffer
	if _, _, _, err := parseMount([]string{"--help"}, &output); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error = %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{"Options may appear before or after", "file:///absolute/path", "https://host:port/url-path", "http+unix://", webDAVUserEnv, webDAVPasswordEnv, "stub-process", "stub-uid", "stub-gid", "log-level", "Options:"} {
		if !bytes.Contains(output.Bytes(), []byte(want)) {
			t.Errorf("mount help does not contain %q:\n%s", want, output.String())
		}
	}
	if bytes.Contains(output.Bytes(), []byte("MEDIASTUB_WEBDAV")) {
		t.Fatalf("mount help contains obsolete environment name:\n%s", output.String())
	}
	if bytes.Contains(output.Bytes(), []byte("debug-fuse")) {
		t.Fatalf("mount help contains obsolete debug-fuse option:\n%s", output.String())
	}
}

func TestParseMountRequiresTwoArguments(t *testing.T) {
	if _, _, _, err := parseMount([]string{"file:///srv/media"}, io.Discard); err == nil {
		t.Fatal("missing mountpoint was accepted")
	}
}

func TestParseMountRejectsInvalidLimits(t *testing.T) {
	for _, args := range [][]string{
		{"--probe-max-requests", "0", "file:///srv/media", "/data/media"},
		{"--plan-cache-entries", "-1", "file:///srv/media", "/data/media"},
		{"--attr-ttl", "0s", "file:///srv/media", "/data/media"},
		{"--probe-max-read", "1MiB", "--probe-window-size", "2MiB", "file:///srv/media", "/data/media"},
		{"--stub-process", "", "file:///srv/media", "/data/media"},
		{"--stub-process", "[", "file:///srv/media", "/data/media"},
		{"--stub-uid", "user", "file:///srv/media", "/data/media"},
		{"--stub-uid", "-1", "file:///srv/media", "/data/media"},
		{"--stub-uid", "4294967296", "file:///srv/media", "/data/media"},
		{"--stub-gid", "group", "file:///srv/media", "/data/media"},
		{"--log-level", "trace", "file:///srv/media", "/data/media"},
	} {
		if _, _, _, err := parseMount(args, io.Discard); err == nil {
			t.Fatalf("invalid arguments were accepted: %q", args)
		}
	}
}

func TestByteSizeString(t *testing.T) {
	for value, want := range map[byteSize]string{16 << 20: "16MiB", 256 << 10: "256KiB", 123: "123"} {
		value := value
		if got := value.String(); got != want {
			t.Errorf("byteSize(%d).String() = %q, want %q", value, got, want)
		}
	}
}

func TestWebDAVCredentials(t *testing.T) {
	t.Setenv(webDAVUserEnv, "alice")
	t.Setenv(webDAVPasswordEnv, "secret")
	for _, remote := range []string{
		"http://example.test/media/",
		"https://example.test/media/",
		"http+unix://%2Frun%2Fwebdav.sock/media/",
	} {
		user, password, err := webDAVCredentials(remote)
		if err != nil || user != "alice" || password != "secret" {
			t.Fatalf("webDAVCredentials(%q) = %q, %q, %v", remote, user, password, err)
		}
	}

	user, password, err := webDAVCredentials("file:///srv/media")
	if err != nil || user != "" || password != "" {
		t.Fatalf("file credentials = %q, %q, %v; want empty", user, password, err)
	}
}

func TestWebDAVCredentialsMustBePaired(t *testing.T) {
	oldPassword, hadPassword := os.LookupEnv(webDAVPasswordEnv)
	if err := os.Unsetenv(webDAVPasswordEnv); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadPassword {
			_ = os.Setenv(webDAVPasswordEnv, oldPassword)
		} else {
			_ = os.Unsetenv(webDAVPasswordEnv)
		}
	})
	t.Setenv(webDAVUserEnv, "alice")
	for _, remote := range []string{"http://example.test/media/", "https://example.test/media/"} {
		if _, _, err := webDAVCredentials(remote); err == nil {
			t.Fatalf("unpaired WebDAV credential was accepted for %q", remote)
		}
	}
}
