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

// NewRemote constructs an Origin from a file, HTTP(S) WebDAV or Unix-socket
// WebDAV URL. WebDAV credentials are supplied separately so they never need to
// appear in the URL or process arguments.
func NewRemote(remote, user, password string) (Origin, error) {
	return NewRemoteWithAuth(remote, Auth{User: user, Password: password})
}

// NewRemoteWithAuth constructs an Origin using explicit WebDAV authentication.
func NewRemoteWithAuth(remote string, auth Auth) (Origin, error) {
	return NewRemoteWithAuthAndTimeouts(remote, auth, DefaultTimeouts)
}

// NewRemoteWithAuthAndTimeouts constructs a remote with request-level timeout policy.
func NewRemoteWithAuthAndTimeouts(remote string, auth Auth, timeouts Timeouts) (Origin, error) {
	if err := auth.Validate(); err != nil {
		return nil, err
	}
	if strings.HasPrefix(remote, "http+unix://") {
		return newUnixWebDAVWithAuthAndTimeouts(remote, auth, timeouts)
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
		return newOwnedWebDAVWithAuthAndTimeouts(u.String(), auth, defaultHTTPTransport(), timeouts)
	default:
		return nil, fmt.Errorf("unsupported remote scheme %q; want file, http, https or http+unix", u.Scheme)
	}
}

func defaultHTTPTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout: 10 * time.Second, KeepAlive: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 0
	transport.IdleConnTimeout = 90 * time.Second
	return transport
}

func newUnixWebDAV(remote, user, password string) (Origin, error) {
	return newUnixWebDAVWithAuth(remote, Auth{User: user, Password: password})
}

func newUnixWebDAVWithAuth(remote string, auth Auth) (Origin, error) {
	return newUnixWebDAVWithAuthAndTimeouts(remote, auth, DefaultTimeouts)
}

func newUnixWebDAVWithAuthAndTimeouts(remote string, auth Auth, timeouts Timeouts) (Origin, error) {
	return newUnixWebDAVWithNetworkTransportAuthAndTimeouts(remote, auth, defaultHTTPTransport(), timeouts)
}

func newUnixWebDAVWithNetworkTransport(remote, user, password string, networkTransport *http.Transport) (Origin, error) {
	return newUnixWebDAVWithNetworkTransportAuth(remote, Auth{User: user, Password: password}, networkTransport)
}

func newUnixWebDAVWithNetworkTransportAuth(remote string, auth Auth, networkTransport *http.Transport) (Origin, error) {
	return newUnixWebDAVWithNetworkTransportAuthAndTimeouts(remote, auth, networkTransport, DefaultTimeouts)
}

func newUnixWebDAVWithNetworkTransportAuthAndTimeouts(remote string, auth Auth, networkTransport *http.Transport, timeouts Timeouts) (Origin, error) {
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
	return newOwnedWebDAVWithAuthAndTimeouts(base.String(), auth, transport, timeouts)
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
	return newOwnedWebDAVWithAuth(baseURL, Auth{User: user, Password: password}, transport)
}

func newOwnedWebDAVWithAuth(baseURL string, auth Auth, transport ownedTransport) (Origin, error) {
	return newOwnedWebDAVWithAuthAndTimeouts(baseURL, auth, transport, DefaultTimeouts)
}

func newOwnedWebDAVWithAuthAndTimeouts(baseURL string, auth Auth, transport ownedTransport, timeouts Timeouts) (Origin, error) {
	client := &http.Client{Transport: transport}
	webdav, err := NewWebDAVWithAuthAndTimeouts(baseURL, auth, client, timeouts)
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
