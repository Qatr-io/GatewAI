package storage_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"testing"

	"gatewai/relay/internal/config"
	"gatewai/relay/internal/storage"
)

func generateTestCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func baseS3Config() config.S3Config {
	return config.S3Config{
		Endpoint:  "https://s3.example.com",
		Region:    "us-east-1",
		AccessKey: "AKID",
		SecretKey: "SECRET",
		Bucket:    "test-bucket",
	}
}

func TestNewS3Client_NoCABundle_Succeeds(t *testing.T) {
	_, err := storage.NewS3Client(baseS3Config(), config.EncryptionConfig{})
	if err != nil {
		t.Fatalf("expected no error without CA bundle, got: %v", err)
	}
}

func TestNewS3Client_CABundle_ValidPEM_Succeeds(t *testing.T) {
	pemData := generateTestCAPEM(t)
	f, err := os.CreateTemp(t.TempDir(), "ca-*.pem")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(pemData)
	f.Close()

	cfg := baseS3Config()
	cfg.CABundle = f.Name()

	_, err = storage.NewS3Client(cfg, config.EncryptionConfig{})
	if err != nil {
		t.Fatalf("expected no error with valid CA bundle, got: %v", err)
	}
}

func TestNewS3Client_CABundle_FileNotFound_Error(t *testing.T) {
	cfg := baseS3Config()
	cfg.CABundle = "/nonexistent/path/ca.pem"

	_, err := storage.NewS3Client(cfg, config.EncryptionConfig{})
	if err == nil {
		t.Fatal("expected error for missing CA bundle file, got nil")
	}
}

func TestNewS3Client_CABundle_InvalidPEM_Error(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "ca-*.pem")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("this is not a valid PEM file")
	f.Close()

	cfg := baseS3Config()
	cfg.CABundle = f.Name()

	_, err = storage.NewS3Client(cfg, config.EncryptionConfig{})
	if err == nil {
		t.Fatal("expected error for invalid PEM content, got nil")
	}
}

func TestNewS3Client_MissingCredentials_Error(t *testing.T) {
	cfg := baseS3Config()
	cfg.AccessKey = ""

	_, err := storage.NewS3Client(cfg, config.EncryptionConfig{})
	if err == nil {
		t.Fatal("expected error for missing access_key, got nil")
	}
}
