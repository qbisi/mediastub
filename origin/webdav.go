package origin

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/><d:getcontentlength/><d:getlastmodified/><d:getetag/><d:getcontenttype/></d:prop></d:propfind>`

// WebDAV exposes a read-only WebDAV collection as an Origin.
type WebDAV struct {
	base   *url.URL
	client *http.Client
	auth   Auth
}

// NewWebDAV constructs a WebDAV origin. baseURL may include a collection path.
func NewWebDAV(baseURL, user, password string, client *http.Client) (*WebDAV, error) {
	return NewWebDAVWithAuth(baseURL, Auth{User: user, Password: password}, client)
}

// NewWebDAVWithAuth constructs a WebDAV origin using Basic, Bearer or no authentication.
func NewWebDAVWithAuth(baseURL string, auth Auth, client *http.Client) (*WebDAV, error) {
	if err := auth.Validate(); err != nil {
		return nil, err
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("WebDAV URL must use http or https")
	}
	if client == nil {
		client = &http.Client{}
	}
	clientCopy := *client
	callerRedirect := client.CheckRedirect
	clientCopy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if len(via) > 0 && !sameOrigin(req.URL, via[0].URL) {
			for _, header := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Emby-Token"} {
				req.Header.Del(header)
			}
		}
		if callerRedirect != nil {
			return callerRedirect(req, via)
		}
		return nil
	}
	return &WebDAV{base: base, client: &clientCopy, auth: auth}, nil
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func (w *WebDAV) objectURL(rel string) *url.URL {
	u := *w.base
	if rel == "." || rel == "" {
		return &u
	}
	return u.JoinPath(strings.Split(rel, "/")...)
}

func (w *WebDAV) request(req *http.Request) {
	req.Header.Set("User-Agent", "mediastub/0.1")
	w.auth.apply(req)
}

type davMultiStatus struct {
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href      string        `xml:"href"`
	Propstats []davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}

type davProp struct {
	ResourceType struct {
		Collection *struct{} `xml:"collection"`
	} `xml:"resourcetype"`
	ContentLength string `xml:"getcontentlength"`
	LastModified  string `xml:"getlastmodified"`
	ETag          string `xml:"getetag"`
	ContentType   string `xml:"getcontenttype"`
}

func webDAVStatusError(code int, status string) error {
	switch code {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, status)
	case http.StatusForbidden, http.StatusUnauthorized:
		return fmt.Errorf("permission denied: %s", status)
	default:
		return fmt.Errorf("WebDAV request failed: %s", status)
	}
}

func (w *WebDAV) responsePath(href string) (string, error) {
	u, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	basePath := strings.TrimSuffix(w.base.Path, "/")
	responsePath := strings.TrimSuffix(u.Path, "/")
	if responsePath == basePath {
		return ".", nil
	}
	prefix := basePath + "/"
	if !strings.HasPrefix(responsePath, prefix) {
		return "", fmt.Errorf("WebDAV response %q is outside base path %q", u.Path, w.base.Path)
	}
	rel := strings.TrimPrefix(responsePath, prefix)
	return CleanPath(rel)
}

func (w *WebDAV) propfind(ctx context.Context, rel, depth string) ([]Entry, error) {
	clean, err := CleanPath(rel)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", w.objectURL(clean).String(), bytes.NewBufferString(propfindBody))
	if err != nil {
		return nil, err
	}
	w.request(req)
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, sanitizeURLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, webDAVStatusError(resp.StatusCode, resp.Status)
	}
	var multistatus davMultiStatus
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&multistatus); err != nil {
		return nil, fmt.Errorf("decode WebDAV PROPFIND: %w", err)
	}
	entries := make([]Entry, 0, len(multistatus.Responses))
	for _, response := range multistatus.Responses {
		responseRel, err := w.responsePath(response.Href)
		if err != nil {
			return nil, err
		}
		var prop *davProp
		for i := range response.Propstats {
			if strings.Contains(response.Propstats[i].Status, " 200 ") {
				prop = &response.Propstats[i].Prop
				break
			}
		}
		if prop == nil {
			continue
		}
		size, _ := strconv.ParseInt(strings.TrimSpace(prop.ContentLength), 10, 64)
		modTime, _ := http.ParseTime(strings.TrimSpace(prop.LastModified))
		name := path.Base(responseRel)
		if responseRel == "." {
			name = ""
		}
		entries = append(entries, Entry{
			Path: responseRel, Name: name, Size: size, ModTime: modTime,
			IsDir: prop.ResourceType.Collection != nil, ETag: strings.TrimSpace(prop.ETag),
			ContentType: strings.TrimSpace(prop.ContentType),
		})
	}
	return entries, nil
}

// Stat returns metadata for rel using a Depth: 0 PROPFIND.
func (w *WebDAV) Stat(ctx context.Context, rel string) (Entry, error) {
	clean, err := CleanPath(rel)
	if err != nil {
		return Entry{}, err
	}
	entries, err := w.propfind(ctx, clean, "0")
	if err != nil {
		return Entry{}, err
	}
	for _, entry := range entries {
		if entry.Path == clean {
			return entry, nil
		}
	}
	return Entry{}, ErrNotFound
}

// ReadDir lists direct children using a Depth: 1 PROPFIND.
func (w *WebDAV) ReadDir(ctx context.Context, rel string) ([]Entry, error) {
	clean, err := CleanPath(rel)
	if err != nil {
		return nil, err
	}
	entries, err := w.propfind(ctx, clean, "1")
	if err != nil {
		return nil, err
	}
	out := entries[:0]
	for _, entry := range entries {
		if entry.Path == clean {
			continue
		}
		parent := path.Dir(entry.Path)
		if clean == "." {
			parent = path.Dir(entry.Path)
		}
		if parent == clean || (clean == "." && parent == ".") {
			out = append(out, entry)
		}
	}
	return out, nil
}

// Open returns a random-access HTTP Range object.
func (w *WebDAV) Open(ctx context.Context, entry Entry) (Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if entry.IsDir {
		return nil, ErrIsDir
	}
	return &webDAVObject{origin: w, entry: entry}, nil
}

// Put writes an object directly to its final WebDAV path.
func (w *WebDAV) Put(ctx context.Context, rel string, src io.Reader, size int64, contentType string) (Entry, error) {
	clean, err := CleanPath(rel)
	if err != nil {
		return Entry{}, err
	}
	if clean == "." || size < 0 {
		return Entry{}, errors.New("invalid WebDAV PUT target or size")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, w.objectURL(clean).String(), src)
	if err != nil {
		return Entry{}, err
	}
	w.request(req)
	req.ContentLength = size
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return Entry{}, sanitizeURLError(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return Entry{}, webDAVStatusError(resp.StatusCode, resp.Status)
	}
	return w.Stat(ctx, clean)
}

// Close does not close a caller-owned HTTP client.
func (w *WebDAV) Close() error { return nil }

type webDAVObject struct {
	origin *WebDAV
	entry  Entry
}

func (o *webDAVObject) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 || off >= o.entry.Size {
		return 0, io.EOF
	}
	want := min(int64(len(p)), o.entry.Size-off)
	end := off + want - 1
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.origin.objectURL(o.entry.Path).String(), nil)
	if err != nil {
		return 0, err
	}
	o.origin.request(req)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))
	if o.entry.ETag != "" {
		req.Header.Set("If-Range", o.entry.ETag)
	}
	resp, err := o.origin.client.Do(req)
	if err != nil {
		return 0, sanitizeURLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && !(resp.StatusCode == http.StatusOK && off == 0 && want == o.entry.Size) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return 0, webDAVStatusError(resp.StatusCode, resp.Status)
	}
	if resp.StatusCode == http.StatusPartialContent {
		wantRange := fmt.Sprintf("bytes %d-%d/", off, end)
		if !strings.HasPrefix(resp.Header.Get("Content-Range"), wantRange) {
			return 0, fmt.Errorf("invalid WebDAV Content-Range %q, want prefix %q", resp.Header.Get("Content-Range"), wantRange)
		}
	}
	n, readErr := io.ReadFull(resp.Body, p[:want])
	if errors.Is(readErr, io.ErrUnexpectedEOF) {
		readErr = io.EOF
	}
	if readErr != nil {
		return n, readErr
	}
	if n != len(p) {
		return n, io.EOF
	}
	return n, nil
}

func sanitizeURLError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || urlErr.URL == "" {
		return err
	}
	requestURL, parseErr := url.Parse(urlErr.URL)
	if parseErr != nil || requestURL.RawQuery == "" {
		return err
	}
	requestURL.RawQuery = ""
	requestURL.ForceQuery = false
	clean := *urlErr
	clean.URL = requestURL.String()
	return &clean
}

func (o *webDAVObject) Close() error { return nil }

var _ MutableOrigin = (*WebDAV)(nil)
