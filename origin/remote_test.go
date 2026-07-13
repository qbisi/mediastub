package origin

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestNewRemoteFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "movie.mkv"), []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := (&url.URL{Scheme: "file", Path: root}).String()
	upstream, err := NewRemote(remote, "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	entry, err := upstream.Stat(context.Background(), "movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Size != 5 {
		t.Fatalf("entry size = %d, want 5", entry.Size)
	}
}

func TestNewRemoteHTTPS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "alice" || password != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != "PROPFIND" || r.URL.Path != "/library/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, multiStatus(davResponseXML("/library/", true, 0)))
	}))
	defer server.Close()

	defaultTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = defaultTransport }()

	upstream, err := NewRemote(server.URL+"/library/", "alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	entry, err := upstream.Stat(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if !entry.IsDir || entry.Path != "." {
		t.Fatalf("unexpected HTTPS root entry: %+v", entry)
	}
}

func TestNewRemoteHTTPUnix(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "webdav.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "alice" || password != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != "PROPFIND" || r.URL.Path != "/library/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, multiStatus(davResponseXML("/library/", true, 0)))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})

	remote := "http+unix://" + url.PathEscape(socketPath) + "/library/"
	upstream, err := NewRemote(remote, "alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	entry, err := upstream.Stat(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if !entry.IsDir || entry.Path != "." {
		t.Fatalf("unexpected root entry: %+v", entry)
	}
}

func TestNewRemoteHTTPUnixFollowsExternalHTTPSRedirect(t *testing.T) {
	content := []byte("redirected media bytes")
	external := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/signed-object" || r.Header.Get("Range") != "bytes=3-10" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("WebDAV credentials leaked to redirect target: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 3-10/%d", len(content)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[3:11])
	}))
	defer external.Close()

	socketPath := filepath.Join(t.TempDir(), "webdav.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PROPFIND":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprint(w, multiStatus(davResponseXML("/library/movie.mkv", false, len(content))))
		case http.MethodGet:
			http.Redirect(w, r, external.URL+"/signed-object", http.StatusFound)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})

	networkTransport := external.Client().Transport.(*http.Transport).Clone()
	remote := "http+unix://" + url.PathEscape(socketPath) + "/library/"
	upstream, err := newUnixWebDAVWithNetworkTransport(remote, "alice", "secret", networkTransport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	entry, err := upstream.Stat(context.Background(), "movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	object, err := upstream.Open(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	defer object.Close()
	buf := make([]byte, 8)
	n, err := object.ReadAt(context.Background(), buf, 3)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != len(buf) || string(buf) != string(content[3:11]) {
		t.Fatalf("redirected ReadAt = %q, %d", buf, n)
	}
}

func TestNewRemoteRejectsInvalidURLs(t *testing.T) {
	for _, remote := range []string{
		"relative/path",
		"file://server/path",
		"http:///missing-host",
		"https:///missing-host",
		"http://user:password@example.test/",
		"https://user:password@example.test/",
		"http+unix://relative/socket",
	} {
		t.Run(remote, func(t *testing.T) {
			if upstream, err := NewRemote(remote, "", ""); err == nil {
				_ = upstream.Close()
				t.Fatalf("NewRemote(%q) unexpectedly succeeded", remote)
			}
		})
	}
}
