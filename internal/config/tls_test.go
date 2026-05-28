// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// SPDX-License-Identifier: MIT

package config_test

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
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/astockwell/pkgmirror/internal/config"
)

// TestTLSEnabled covers the simple boolean knob — both env vars must
// be set for TLS to engage; one alone is treated as misconfiguration
// (caught at boot by cmd/pkgmirror/main.go).
func TestTLSEnabled(t *testing.T) {
	cases := []struct {
		cert, key string
		want      bool
	}{
		{"", "", false},
		{"/a", "", false},
		{"", "/b", false},
		{"/a", "/b", true},
	}
	for _, tc := range cases {
		c := config.Config{TLSCertFile: tc.cert, TLSKeyFile: tc.key}
		if got := c.TLSEnabled(); got != tc.want {
			t.Errorf("TLSEnabled(cert=%q, key=%q): want %v got %v", tc.cert, tc.key, tc.want, got)
		}
	}
}

// TestTLSServerActuallyServes is the load-bearing check: with a
// freshly-generated self-signed cert + key on disk, an http.Server
// started via ListenAndServeTLS (the same call cmd/pkgmirror uses)
// answers a real HTTPS request. If the stdlib ever changes its
// cert/key file format expectations, this fails loud at test time
// rather than silently at the operator's first deployment.
func TestTLSServerActuallyServes(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeSelfSignedCert(t, certPath, keyPath)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	t.Cleanup(func() { _ = srv.Close() })

	done := make(chan error, 1)
	go func() {
		done <- srv.ServeTLS(ln, certPath, keyPath)
	}()

	// Client trusts our self-signed cert via InsecureSkipVerify —
	// this isn't about cert validation, it's about confirming the
	// listener actually speaks TLS.
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // test-only
		Timeout:   3 * time.Second,
	}
	resp, err := client.Get("https://" + addr)
	if err != nil {
		t.Fatalf("https GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if resp.TLS == nil {
		t.Fatalf("response not over TLS")
	}
}

// writeSelfSignedCert writes a fresh ECDSA-P256 self-signed cert + key
// pair to certPath / keyPath. Valid for one hour for 127.0.0.1.
func writeSelfSignedCert(t *testing.T, certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "pkgmirror-test"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}
