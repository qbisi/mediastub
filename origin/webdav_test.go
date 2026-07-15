package origin

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestWebDAVPropfindAndRangeRead(t *testing.T) {
	content := []byte("abcdefghijklmnopqrstuvwxyz")
	var mu sync.Mutex
	var rangeHeaders, ifRangeHeaders []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:secret")) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case "PROPFIND":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			if r.Header.Get("Depth") == "1" {
				fmt.Fprint(w, multiStatus(
					davResponseXML("/dav/media/", true, 0),
					davResponseXML("/dav/media/movie.mkv", false, len(content)),
					davResponseXML("/dav/media/sub/", true, 0),
				))
				return
			}
			if strings.HasSuffix(r.URL.Path, "movie.mkv") {
				fmt.Fprint(w, multiStatus(davResponseXML("/dav/media/movie.mkv", false, len(content))))
				return
			}
			fmt.Fprint(w, multiStatus(davResponseXML("/dav/media/", true, 0)))
		case http.MethodGet:
			mu.Lock()
			rangeHeaders = append(rangeHeaders, r.Header.Get("Range"))
			ifRangeHeaders = append(ifRangeHeaders, r.Header.Get("If-Range"))
			mu.Unlock()
			var start, end int
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
			w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[start : end+1])
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	webdav, err := NewWebDAV(server.URL+"/dav/media/", "alice", "secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	entries, err := webdav.ReadDir(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "movie.mkv" || entries[1].Path != "sub" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	entry, err := webdav.Stat(context.Background(), "movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Size != int64(len(content)) || entry.ETag != `"version-1"` {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	object, err := webdav.Open(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	n, err := object.ReadAt(context.Background(), buf, 7)
	if err != nil || n != 5 || string(buf) != "hijkl" {
		t.Fatalf("ReadAt = %q, %d, %v", buf, n, err)
	}
	if err := object.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(rangeHeaders) != 1 || rangeHeaders[0] != "bytes=7-11" {
		t.Fatalf("Range headers = %q", rangeHeaders)
	}
	if len(ifRangeHeaders) != 1 || ifRangeHeaders[0] != `"version-1"` {
		t.Fatalf("If-Range headers = %q", ifRangeHeaders)
	}
}

func TestWebDAVRejectsWrongContentRange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-2/3")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "abc")
	}))
	defer server.Close()
	webdav, err := NewWebDAV(server.URL+"/", "", "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	object, err := webdav.Open(context.Background(), Entry{Path: "x", Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := object.ReadAt(context.Background(), make([]byte, 3), 4); err == nil || !strings.Contains(err.Error(), "Content-Range") {
		t.Fatalf("wrong Content-Range error = %v", err)
	}
}

func TestWebDAVBearerPutAndStat(t *testing.T) {
	var content []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodPut:
			content, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
		case "PROPFIND":
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprint(w, multiStatus(davResponseXML("/dav/movie.nfo", false, len(content))))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	webdav, err := NewWebDAVWithAuth(server.URL+"/dav/", Auth{BearerToken: "secret-token"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("<movie/>")
	entry, err := webdav.Put(context.Background(), "movie.nfo", strings.NewReader(string(want)), int64(len(want)), "application/xml")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(want) || entry.Size != int64(len(want)) {
		t.Fatalf("content=%q entry=%+v", content, entry)
	}
}

func TestWebDAVRedirectDoesNotForwardAuthorization(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("redirect target received Authorization %q", auth)
		}
		w.Header().Set("Content-Range", "bytes 0-2/3")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "abc")
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer source-token" {
			t.Errorf("source missing Bearer token")
		}
		http.Redirect(w, r, target.URL+"/object", http.StatusFound)
	}))
	defer source.Close()
	webdav, err := NewWebDAVWithAuth(source.URL+"/", Auth{BearerToken: "source-token"}, source.Client())
	if err != nil {
		t.Fatal(err)
	}
	object, err := webdav.Open(context.Background(), Entry{Path: "movie.mkv", Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := object.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "abc" {
		t.Fatalf("body = %q", buf)
	}
}

func TestSanitizeURLErrorRedactsSignedQuery(t *testing.T) {
	cause := errors.New("transport failed")
	err := sanitizeURLError(&url.Error{
		Op: "Get", URL: "https://objects.example/movie.mkv?X-Amz-Signature=secret&X-Amz-Expires=900", Err: cause,
	})
	if !errors.Is(err, cause) {
		t.Fatalf("sanitized error lost wrapped cause: %v", err)
	}
	got := err.Error()
	if strings.Contains(got, "secret") || strings.Contains(got, "X-Amz-") {
		t.Fatalf("sanitized error leaked signed query: %q", got)
	}
	if !strings.Contains(got, "https://objects.example/movie.mkv") {
		t.Fatalf("sanitized error lost useful URL path: %q", got)
	}
}

func multiStatus(responses ...string) string {
	return `<?xml version="1.0" encoding="utf-8"?><d:multistatus xmlns:d="DAV:">` + strings.Join(responses, "") + `</d:multistatus>`
}

func davResponseXML(href string, collection bool, size int) string {
	resourceType := ""
	if collection {
		resourceType = "<d:collection/>"
	}
	return fmt.Sprintf(`<d:response><d:href>%s</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><d:resourcetype>%s</d:resourcetype><d:getcontentlength>%d</d:getcontentlength><d:getlastmodified>Mon, 13 Jul 2026 08:00:00 GMT</d:getlastmodified><d:getetag>&quot;version-1&quot;</d:getetag><d:getcontenttype>video/x-matroska</d:getcontenttype></d:prop></d:propstat></d:response>`, href, resourceType, size)
}
