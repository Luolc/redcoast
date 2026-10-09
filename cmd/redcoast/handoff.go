package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/Luolc/redcoast/claude"
	"github.com/Luolc/redcoast/session"
	"github.com/cloudflare/tableflip"
)

// handoffTimeout bounds the new process's startup in a handoff, as the unit's
// TimeoutStartSec bounds it at a start.
const handoffTimeout = 240 * time.Second

// drainTimeout is how long the old process lets inference requests and tunnels in
// progress run after a handoff before it cuts them.
const drainTimeout = 30 * time.Minute

// listeners are the gateway's sockets. A process started by a handoff inherits them
// from the old one; otherwise they are opened here. Both processes accept on them
// until the old one stops, so no connection is refused in between.
type listeners struct {
	reverse, forward, health, dashboard, session, admin, sessionTCP net.Listener // sessionTCP is nil without --session-listen
}

// listen opens or inherits every listener cfg names. With cfg.tls every TCP listener
// speaks TLS; a new process wraps the inherited sockets with the certificate it read.
func listen(upg *tableflip.Upgrader, cfg config) (listeners, error) {
	var l listeners
	var err error
	for _, tcp := range []struct {
		address  string
		listener *net.Listener
	}{{cfg.listen, &l.reverse}, {cfg.forwardListen, &l.forward}, {cfg.healthListen, &l.health}, {cfg.dashboardListen, &l.dashboard}, {cfg.sessionListen, &l.sessionTCP}} {
		if tcp.address == "" {
			continue
		}
		if *tcp.listener, err = upg.Listen("tcp", tcp.address); err != nil {
			return listeners{}, err
		}
		if cfg.tls != nil {
			*tcp.listener = tls.NewListener(*tcp.listener, cfg.tls)
		}
	}
	unix := func(_, path string) (net.Listener, error) { return session.ListenUnixSocket(path) }
	if l.session, err = upg.ListenWithCallback("unix", cfg.socket, unix); err != nil {
		return listeners{}, err
	}
	if l.admin, err = upg.ListenWithCallback("unix", cfg.adminSocket, unix); err != nil {
		return listeners{}, err
	}
	return l, nil
}

// serveUntilHandoff returns the context the servers run under: it ends with ctx, or
// with a claude.Handoff cause once handedOff closes, which lets requests in progress
// run for drain more (until ctx ends). stop releases it after the servers return.
func serveUntilHandoff(ctx context.Context, handedOff <-chan struct{}, drain time.Duration) (serving context.Context, stop func()) {
	serving, cancel := context.WithCancelCause(ctx)
	go func() {
		select {
		case <-handedOff:
		case <-serving.Done():
			return
		}
		requests, cut := context.WithTimeout(ctx, drain)
		defer cut()
		log.Printf("handoff: the new process is ready; draining for at most %s", drain)
		cancel(claude.Handoff{Requests: requests})
		<-requests.Done()
	}()
	return serving, func() { cancel(nil) }
}

// listenAndNotify opens or inherits the listeners, then tells systemd and, after a
// handoff, the old process that this one is ready; the old one then stops accepting
// and drains.
func listenAndNotify(upg *tableflip.Upgrader, cfg config) (listeners, error) {
	l, err := listen(upg, cfg)
	if err != nil {
		return listeners{}, err
	}
	if err := notifyReady(); err != nil {
		return listeners{}, fmt.Errorf("systemd notification: %v", err)
	}
	return l, upg.Ready()
}

// afterOldProcess runs run once the old process of a handoff has exited, so that the
// two never run backups or retention on the store at once.
func afterOldProcess(upg *tableflip.Upgrader, run func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		if upg.WaitForParent(ctx) != nil {
			return nil
		}
		return run(ctx)
	}
}
