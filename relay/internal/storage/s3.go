package storage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"gatewai/relay/internal/config"
	"gatewai/relay/internal/crypto"
	"gatewai/relay/internal/metrics"
)

// S3Client wraps the AWS SDK v2 S3 client for any S3-compatible object storage.
type S3Client struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
	encKey   []byte // nil = encryption disabled
}

// NewS3Client builds an S3 client from the provided config.
// When cfg.CABundle is set, TLS verification uses the certificates in that PEM
// file instead of (not in addition to) the system certificate pool.
func NewS3Client(cfg config.S3Config, encCfg config.EncryptionConfig) (*S3Client, error) {
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("s3: access_key and secret_key are required")
	}

	encKey, err := crypto.ParseKey(encCfg.Key)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}

	opts := s3.Options{
		BaseEndpoint: aws.String(cfg.Endpoint),
		Region:       cfg.Region,
		Credentials: aws.NewCredentialsCache(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		),
		UsePathStyle:               cfg.UsePathStyle,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}

	tlsCfg := &tls.Config{}
	needCustomTransport := false

	if cfg.SSLInsecure {
		tlsCfg.InsecureSkipVerify = true
		needCustomTransport = true
	} else if cfg.CABundle != "" {
		pem, err := os.ReadFile(cfg.CABundle)
		if err != nil {
			return nil, fmt.Errorf("s3: reading CA bundle %q: %w", cfg.CABundle, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("s3: no valid certificates found in %q", cfg.CABundle)
		}
		tlsCfg.RootCAs = pool
		needCustomTransport = true
	}

	if needCustomTransport {
		opts.HTTPClient = &http.Client{
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		}
	}

	client := s3.New(opts)

	return &S3Client{client: client, uploader: manager.NewUploader(client), bucket: cfg.Bucket, encKey: encKey}, nil
}

// GetObject downloads an object and returns a streaming body, content-length
// (-1 if unknown), content-type, and any error. The caller must close the body.
// If encryption is enabled the stream is transparently decrypted.
func (c *S3Client) GetObject(ctx context.Context, key string) (io.ReadCloser, int64, string, error) {
	start := time.Now()
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	metrics.S3OperationDuration.WithLabelValues("get").Observe(time.Since(start).Seconds())
	if err != nil {
		metrics.S3ErrorsTotal.WithLabelValues("get").Inc()
		return nil, 0, "", fmt.Errorf("getting S3 object %q: %w", key, err)
	}

	ct := ""
	if out.ContentType != nil {
		ct = *out.ContentType
	}

	body := crypto.Decrypt(c.encKey, out.Body)
	// Decrypted size is unknown (slightly smaller than ciphertext); pass -1.
	return body, -1, ct, nil
}

// DeleteObject removes an object from the configured bucket.
func (c *S3Client) DeleteObject(ctx context.Context, key string) error {
	start := time.Now()
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	metrics.S3OperationDuration.WithLabelValues("delete").Observe(time.Since(start).Seconds())
	if err != nil {
		metrics.S3ErrorsTotal.WithLabelValues("delete").Inc()
		return fmt.Errorf("deleting S3 object %q: %w", key, err)
	}
	return nil
}

// PutObject stores data at key in the configured bucket.
// If encryption is enabled the data is encrypted before upload.
// Uses multipart upload to support non-seekable streams (io.Pipe).
func (c *S3Client) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	start := time.Now()
	enc := crypto.Encrypt(c.encKey, body)
	defer enc.Close()

	_, err := c.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        enc,
		ContentType: aws.String(contentType),
	})
	metrics.S3OperationDuration.WithLabelValues("put").Observe(time.Since(start).Seconds())
	if err != nil {
		metrics.S3ErrorsTotal.WithLabelValues("put").Inc()
		return fmt.Errorf("putting S3 object %q: %w", key, err)
	}
	return nil
}
