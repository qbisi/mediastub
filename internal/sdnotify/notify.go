// Package sdnotify implements the subset of sd_notify needed by mediastub.
package sdnotify

import (
	"errors"
	"net"
	"os"
	"strings"
)

// Ready tells systemd that initial synchronization completed.
func Ready(status string) error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	if strings.HasPrefix(socket, "@") {
		socket = "\x00" + strings.TrimPrefix(socket, "@")
	}
	addr := &net.UnixAddr{Name: socket, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte("READY=1\nSTATUS=" + strings.ReplaceAll(status, "\n", " ")))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
