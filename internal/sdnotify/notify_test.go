package sdnotify

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReady(t *testing.T) {
	name := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	t.Setenv("NOTIFY_SOCKET", name)
	if err := Ready("initial\ncomplete"); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := string(buf[:n])
	if !strings.Contains(got, "READY=1") || !strings.Contains(got, "STATUS=initial complete") {
		t.Fatalf("notification = %q", got)
	}
}
