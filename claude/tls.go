package claude

import "crypto/tls"

// ServerTLS is the configuration of the gateway's TLS listeners: TLS 1.2 or later with
// cert, and HTTP/1.1 as the only protocol, since the forward proxy hijacks the
// connection of each CONNECT and HTTP/2 has no connection to hand over. A client that
// offers only h2 fails the handshake.
func ServerTLS(cert tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
}
