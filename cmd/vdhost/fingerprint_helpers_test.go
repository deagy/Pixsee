package main

// R2 helper-level tests for AC-7: the ephemeral fingerprint the host publishes
// must be exactly the value transport.ClientTLSConfigForFingerprint compares
// against the wire, and the announcement must label it and reference the
// client -fingerprint flag. (The subprocess proof that the real startup path
// prints it is TestVdhostAnnouncesEphemeralCertFingerprint.)

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"strings"
	"testing"

	"virtualdesktop/internal/transport"
)

func TestEphemeralFingerprintMatchesWirePin(t *testing.T) {
	cert, err := transport.EphemeralServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := transport.ServerTLSConfig(cert)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := ephemeralFingerprint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(cert.Certificate[0]) // leaf DER == PeerCertificates[0].Raw on the wire
	if fp != want {
		t.Fatalf("ephemeralFingerprint = %x, want sha256(leaf DER) = %x", fp, want)
	}
	// A client pinned with the published value must trust this config's cert.
	if _, err := transport.ClientTLSConfigForFingerprint(fp); err != nil {
		t.Fatalf("published pin %x rejected by the client pinning constructor: %v", fp, err)
	}
}

func TestEphemeralFingerprintRejectsEmptyConfig(t *testing.T) {
	if _, err := ephemeralFingerprint(nil); err == nil {
		t.Fatal("nil TLS config must not yield a fingerprint")
	}
	if _, err := ephemeralFingerprint(&tls.Config{}); err == nil {
		t.Fatal("certificate-less TLS config must not yield a fingerprint")
	}
}

func TestAnnounceEphemeralCertWording(t *testing.T) {
	fp := sha256.Sum256([]byte("certificate-under-test"))
	var buf bytes.Buffer
	announceEphemeralCert(&buf, fp)
	out := buf.String()
	if !fingerprintLineRE.MatchString(out) {
		t.Fatalf("announcement must print the labelled 64-hex SHA-256 pin; got %q", out)
	}
	if !strings.Contains(out, "-fingerprint") {
		t.Fatalf("announcement must reference the client -fingerprint flag; got %q", out)
	}
	if !strings.Contains(out, ephemeralCertValidity.String()) {
		t.Fatalf("announcement must state the certificate validity (%v); got %q", ephemeralCertValidity, out)
	}
	if strings.Contains(strings.ToUpper(out), "PRIVATE KEY") {
		t.Fatalf("announcement must never surface key material; got %q", out)
	}
}

// TestLoadConfigMarksEphemeralMode proves the config surface (no -ca/-key)
// computes exactly the fingerprint the startup announcement prints, so the
// printed pin and the served certificate cannot drift apart.
func TestLoadConfigMarksEphemeralMode(t *testing.T) {
	sanitizeVdhostEnv(t)
	cfg, err := loadConfig([]string{"-addr", "127.0.0.1:0", "-no-auth"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ephemeralCert {
		t.Fatal("no -ca/-key must select ephemeral certificate mode")
	}
	want, err := ephemeralFingerprint(cfg.tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.certFingerprint != want {
		t.Fatalf("stored fingerprint %x != fingerprint of served config %x", cfg.certFingerprint, want)
	}

	// With an operator-supplied pair, ephemeral mode must stay off.
	caPath, keyPath := writeKeyPair(t)
	cfg, err = loadConfig([]string{"-addr", "127.0.0.1:0", "-no-auth", "-ca", caPath, "-key", keyPath})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ephemeralCert {
		t.Fatal("operator -ca/-key must not be treated as ephemeral mode")
	}
}
