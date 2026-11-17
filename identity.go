package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// identity loads (or creates) the self-signed client cert LocalSend >=1.17
// demands via mutual TLS; its SHA-256 is our device fingerprint.
func identity() (tls.Certificate, string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return tls.Certificate{}, "", err
	}
	dir = filepath.Join(dir, "tsdrop")
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		if err := newIdentity(dir, certPath, keyPath); err != nil {
			return tls.Certificate{}, "", err
		}
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	sum := sha256.Sum256(cert.Certificate[0])
	return cert, strings.ToUpper(hex.EncodeToString(sum[:])), nil
}

func newIdentity(dir, certPath, keyPath string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "tsdrop " + host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}
