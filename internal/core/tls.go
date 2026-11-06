package core

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// Enabled reports whether any TLS option was set explicitly.
func (t TLS) Enabled() bool { return t.CAFile != "" || t.Insecure || t.CertFile != "" }

// Config builds a client tls.Config. A zero TLS gives system roots and no client cert.
func (t TLS) Config() (*tls.Config, error) {
	c := &tls.Config{InsecureSkipVerify: t.Insecure} // #nosec G402 -- explicit user opt-in
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("tls caFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls caFile %s: no PEM certificates found", t.CAFile)
		}
		c.RootCAs = pool
	}
	if t.CertFile != "" || t.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		c.Certificates = []tls.Certificate{cert}
	}
	return c, nil
}

// HTTPClient returns a client using this TLS config. It follows redirects and
// relies on the request context for timeouts.
func (t TLS) HTTPClient() (*http.Client, error) {
	c, err := t.Config()
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = c
	return &http.Client{Transport: tr}, nil
}
