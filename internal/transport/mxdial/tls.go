package mxdial

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// ReceiverTLS uses normal system roots and optionally adds an operator's PEM CA
// bundle. Hostname verification is always enabled; it is never a pin to a leaf.
func ReceiverTLS(caFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("cannot read Dial MX CA bundle")
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("Dial MX CA bundle contains no certificates")
	}
	cfg.RootCAs = pool
	return cfg, nil
}
