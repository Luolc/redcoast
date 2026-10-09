package main

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// notifyReady tells systemd the gateway is ready (Type=notify) and is the service's
// main process: one datagram with MAINPID and READY=1 to $NOTIFY_SOCKET. After a
// handoff the new process is not the one systemd started, so it names itself; the unit
// needs NotifyAccess=all to accept that from it. Without the variable, outside systemd or under another type, it
// does nothing. A leading @ names a socket in the abstract namespace.
func notifyReady() error {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return nil
	}
	if rest, abstract := strings.CutPrefix(path, "@"); abstract {
		path = "\x00" + rest
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.Write(fmt.Appendf(nil, "MAINPID=%d\nREADY=1", os.Getpid()))
	return err
}
