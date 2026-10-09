// Package testtls makes the certificates the TLS tests run on: a throwaway CA and a
// server certificate it signs, both generated at run time so that no key sits in the
// source.
package testtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"
)

// ServerName is the DNS name the server certificate covers, besides 127.0.0.1.
const ServerName = "gateway.example.test"

// Server is a server certificate and the pool holding the CA that signed it.
type Server struct {
	CertPEM, KeyPEM []byte
	Certificate     tls.Certificate
	Roots           *x509.CertPool
}

// New makes a CA and a server certificate for ServerName and 127.0.0.1.
func New(t testing.TB) Server {
	t.Helper()
	caKey := newKey(t)
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	if ca, err = x509.ParseCertificate(caDER); err != nil {
		t.Fatal(err)
	}
	key := newKey(t)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: ServerName}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		DNSNames: []string{ServerName}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, key.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	s := Server{CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), Roots: x509.NewCertPool()}
	s.Roots.AddCert(ca)
	if s.Certificate, err = tls.X509KeyPair(s.CertPEM, s.KeyPEM); err != nil {
		t.Fatal(err)
	}
	return s
}

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
