package mxdial_test

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

func TestReceiverTLSCustomCAKeepsHostnameVerification(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := mxdial.ReceiverTLS(path)
	if err != nil || cfg.InsecureSkipVerify {
		t.Fatalf("CA config: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	client.CloseIdleConnections()
	bad := cfg.Clone()
	bad.ServerName = "wrong.example"
	client = &http.Client{Transport: &http.Transport{TLSClientConfig: bad}}
	if resp, err := client.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("incorrect TLS hostname accepted")
	}
	client.CloseIdleConnections()
	if err := os.WriteFile(path, []byte("not PEM"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := mxdial.ReceiverTLS(path); err == nil {
		t.Fatal("invalid CA bundle accepted")
	}
}
