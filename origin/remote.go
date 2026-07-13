package origin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const remoteHTTPTimeout = 2 * time.Minute

// NewRemote constructs an Origin from a file, HTTP(S) WebDAV or Unix-socket
// WebDAV URL. WebDAV credentials are supplied separately so they never need to
// appear in the URL or process arguments.
func NewRemote(remote, user, password string) (Origin, error) {
	if strings.HasPrefix(remote, "http+unix://") {
		return newUnixWebDAV(remote, user, password)
	}
	u, err := url.Parse(remote)
	if err != nil {
		return nil, fmt.Errorf("parse remote: %w", err)
	}
	if u.Fragment != "" {
		return nil, errors.New("remote URL must not contain a fragment")
	}
	if u.User != nil {
		return nil, errors.New("remote URL credentials are not allowed; pass WebDAV credentials separately")
	}
	switch u.Scheme {
	case "file":
		if u.Host != "" || u.RawQuery != "" {
			return nil, errors.New("file remote must have the form file:///absolute/path")
		}
		if !filepath.IsAbs(u.Path) {
			return nil, errors.New("file remote path must be absolute")
		}
		return NewLocal(u.Path)
	case "http", "https":
		if u.Host == "" {
			return nil, errors.New("HTTP(S) WebDAV remote requires a host")
		}
		return newOwnedWebDAV(u.String(), user, password, defaultHTTPTransport())
	default:
		return nil, fmt.Errorf("unsupported remote scheme %q; want file, http, https or http+unix", u.Scheme)
	}
}

func defaultHTTPTransport() *http.Transport {
	return http.DefaultTransport.(*http.Transport).Clone()
}

func newUnixWebDAV(remote, user, password string) (Origin, error) {
	return newUnixWebDAVWithNetworkTransport(remote, user, password, defaultHTTPTransport())
}

func newUnixWebDAVWithNetworkTransport(remote, user, password string, networkTransport *http.Transport) (Origin, error) {
	remainder := strings.TrimPrefix(remote, "http+unix://")
	separator := strings.IndexAny(remainder, "/?#")
	if separator < 0 {
		separator = len(remainder)
	}
	encodedSocket := remainder[:separator]
	if encodedSocket == "" {
		return nil, errors.New("http+unix remote requires a percent-encoded socket path as its host")
	}
	if separator < len(remainder) && remainder[separator] != '/' {
		return nil, errors.New("http+unix remote requires a URL path after the socket host")
	}
	socketPath, err := url.PathUnescape(encodedSocket)
	if err != nil {
		return nil, fmt.Errorf("decode Unix socket path: %w", err)
	}
	if !filepath.IsAbs(socketPath) {
		return nil, errors.New("http+unix socket path must be absolute")
	}
	suffix := "/"
	if separator < len(remainder) {
		suffix = remainder[separator:]
	}
	base, err := url.Parse("http://localhost" + suffix)
	if err != nil {
		return nil, fmt.Errorf("parse http+unix URL path: %w", err)
	}
	if base.Fragment != "" {
		return nil, errors.New("remote URL must not contain a fragment")
	}
	unixTransport := defaultHTTPTransport()
	unixTransport.Proxy = nil
	unixTransport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socketPath)
	}
	transport := &redirectTransport{
		unixHost: base.Host, unix: unixTransport, network: networkTransport,
	}
	return newOwnedWebDAV(base.String(), user, password, transport)
}

// redirectTransport sends requests for the synthetic WebDAV host over the
// Unix socket while allowing absolute redirects to use normal networking.
// OpenList and similar servers commonly redirect ranged GETs to signed HTTPS
// object URLs.
type redirectTransport struct {
	unixHost string
	unix     *http.Transport
	network  *http.Transport
}

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "http" && req.URL.Host == t.unixHost {
		return t.unix.RoundTrip(req)
	}
	return t.network.RoundTrip(req)
}

func (t *redirectTransport) CloseIdleConnections() {
	t.unix.CloseIdleConnections()
	t.network.CloseIdleConnections()
}

type ownedTransport interface {
	http.RoundTripper
	CloseIdleConnections()
}

func newOwnedWebDAV(baseURL, user, password string, transport ownedTransport) (Origin, error) {
	client := &http.Client{Transport: transport, Timeout: remoteHTTPTimeout}
	webdav, err := NewWebDAV(baseURL, user, password, client)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	return &ownedWebDAV{WebDAV: webdav, transport: transport}, nil
}

type ownedWebDAV struct {
	*WebDAV
	transport ownedTransport
}

func (w *ownedWebDAV) Close() error {
	w.transport.CloseIdleConnections()
	return w.WebDAV.Close()
}
