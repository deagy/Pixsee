package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeKeyPair writes a self-signed cert+key to temp files and returns their
// paths. It mirrors what an operator would pass with -ca/-key.
func writeKeyPair(t *testing.T) (caPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	caPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject:      pkix.Name{CommonName: "test"},
		DNSNames:     []string{"localhost"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return caPath, keyPath
}

// writeToken writes a 32-byte token file and returns its path.
func writeToken(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, token, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigTokenAndCAOptional(t *testing.T) {
	caPath, keyPath := writeKeyPair(t)
	tokenPath := writeToken(t)

	cases := []struct {
		name      string
		args      []string
		wantErr   bool
		wantToken bool // true if a non-zero token is expected
	}{
		{
			name:      "no token no ca",
			args:      []string{"-addr", "127.0.0.1:0"},
			wantErr:   false,
			wantToken: false,
		},
		{
			name:      "no token with ca",
			args:      []string{"-addr", "127.0.0.1:0", "-ca", caPath, "-key", keyPath},
			wantErr:   false,
			wantToken: false,
		},
		{
			name:      "token no ca",
			args:      []string{"-addr", "127.0.0.1:0", "-token", tokenPath},
			wantErr:   false,
			wantToken: true,
		},
		{
			name:      "token and ca",
			args:      []string{"-addr", "127.0.0.1:0", "-token", tokenPath, "-ca", caPath, "-key", keyPath},
			wantErr:   false,
			wantToken: true,
		},
		{
			name:    "bad token file",
			args:    []string{"-addr", "127.0.0.1:0", "-token", filepath.Join(t.TempDir(), "missing")},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got cfg=%+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg == nil {
				t.Fatal("nil config")
			}
			if cfg.tlsConfig == nil {
				t.Fatal("tlsConfig is nil; host could not serve TLS")
			}
			if cfg.tlsConfig.MinVersion != tls.VersionTLS13 || cfg.tlsConfig.MaxVersion != tls.VersionTLS13 {
				t.Fatal("host must serve TLS 1.3 only")
			}
			if len(cfg.tlsConfig.Certificates) != 1 || cfg.tlsConfig.Certificates[0].PrivateKey == nil {
				t.Fatal("host must present exactly one key-bearing certificate")
			}
			if cfg.token == [32]byte{} && tc.wantToken {
				t.Fatal("expected a non-zero token")
			}
			if cfg.token != [32]byte{} && !tc.wantToken {
				t.Fatal("expected a zero (no-auth) token")
			}
		})
	}
}

func TestLoadConfigRejectsMalformedToken(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad-token")
	if err := os.WriteFile(bad, []byte("not 32 bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig([]string{"-addr", "127.0.0.1:0", "-token", bad}); err == nil {
		t.Fatal("expected error for malformed token file")
	}
}
