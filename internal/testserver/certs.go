package testserver

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
	"os"
	"path/filepath"
	"time"
)

// Certs is a throwaway CA plus server and client certs, written as PEM files to Dir.
type Certs struct {
	Dir        string
	CAFile     string
	ClientCert string
	ClientKey  string
	Server     tls.Certificate
	Pool       *x509.CertPool
}

// NewCerts generates certs valid for localhost and 127.0.0.1, generated at test
// time so no fixture ever expires.
func NewCerts(dir string) (*Certs, error) {
	caKey, caDER, err := makeCert(nil, nil, &x509.Certificate{
		Subject: pkix.Name{CommonName: "streamx test CA"}, IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	})
	if err != nil {
		return nil, err
	}
	ca, _ := x509.ParseCertificate(caDER)
	c := &Certs{Dir: dir, Pool: x509.NewCertPool()}
	c.Pool.AddCert(ca)

	srvKey, srvDER, err := makeCert(ca, caKey, &x509.Certificate{
		Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return nil, err
	}
	c.Server = tls.Certificate{Certificate: [][]byte{srvDER}, PrivateKey: srvKey}

	cliKey, cliDER, err := makeCert(ca, caKey, &x509.Certificate{
		Subject: pkix.Name{CommonName: "client"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(cliKey)
	if err != nil {
		return nil, err
	}
	c.CAFile = filepath.Join(dir, "ca.pem")
	c.ClientCert = filepath.Join(dir, "client.pem")
	c.ClientKey = filepath.Join(dir, "client-key.pem")
	for path, b := range map[string]*pem.Block{
		c.CAFile:     {Type: "CERTIFICATE", Bytes: caDER},
		c.ClientCert: {Type: "CERTIFICATE", Bytes: cliDER},
		c.ClientKey:  {Type: "EC PRIVATE KEY", Bytes: keyDER},
	} {
		if err := os.WriteFile(path, pem.EncodeToMemory(b), 0o600); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// ServerTLS returns a server config; requireClient turns on mTLS.
func (c *Certs) ServerTLS(requireClient bool) *tls.Config {
	cfg := &tls.Config{Certificates: []tls.Certificate{c.Server}}
	if requireClient {
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = c.Pool
	}
	return cfg
}

// makeCert signs tmpl with parent/parentKey, or self-signs when parent is nil.
func makeCert(parent *x509.Certificate, parentKey *ecdsa.PrivateKey, tmpl *x509.Certificate) (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl.SerialNumber = serial
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	tmpl.NotAfter = time.Now().Add(24 * time.Hour)
	tmpl.KeyUsage |= x509.KeyUsageDigitalSignature
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	return key, der, err
}
