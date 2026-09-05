package transport

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"
)

var ErrCertificatePin = errors.New("server certificate fingerprint mismatch")

func ServerTLSConfig(certificate tls.Certificate) (*tls.Config, error) {
	if len(certificate.Certificate) == 0 || certificate.PrivateKey == nil {
		return nil, errors.New("server certificate and private key are required")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
	}, nil
}

func ClientTLSConfig(serverName string, roots *x509.CertPool) (*tls.Config, error) {
	if serverName == "" || roots == nil {
		return nil, errors.New("server name and certificate roots are required")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		ServerName: serverName,
		RootCAs:    roots,
	}, nil
}

func ClientTLSConfigForCertificate(serverName string, certificate *x509.Certificate) (*tls.Config, error) {
	if certificate == nil {
		return nil, errors.New("server certificate is required")
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return ClientTLSConfig(serverName, roots)
}

func ClientTLSConfigForFingerprint(fingerprint [sha256.Size]byte) (*tls.Config, error) {
	if subtle.ConstantTimeCompare(fingerprint[:], make([]byte, sha256.Size)) == 1 {
		return nil, errors.New("certificate fingerprint is required")
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // Verification is replaced by the exact fingerprint below.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return ErrCertificatePin
			}
			actual := sha256.Sum256(state.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(actual[:], fingerprint[:]) != 1 {
				return ErrCertificatePin
			}
			return nil
		},
	}, nil
}

func Handshake(ctx context.Context, conn *tls.Conn, timeout time.Duration) error {
	if conn == nil {
		return errors.New("TLS connection is required")
	}
	operationCtx, cancel, err := boundedContext(ctx, timeout)
	if err != nil {
		return err
	}
	defer cancel()
	if err := conn.HandshakeContext(operationCtx); err != nil {
		return fmt.Errorf("TLS handshake: %w", err)
	}
	return nil
}

func boundedContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if timeout <= 0 {
		return nil, nil, errors.New("timeout must be positive")
	}
	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	return operationCtx, cancel, nil
}
