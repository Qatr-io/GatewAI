package storage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/crypto"
	"gatewai/gateway/internal/metrics"
)

// S3Client wraps the AWS SDK v2 S3 client for any S3-compatible object storage.
type S3Client struct {
	s3       *s3.Client
	uploader *manager.Uploader
	bucket   string
	encKey   []byte // nil = encryption disabled
}

// S3JobEntry groups all S3 object keys belonging to a single job.
type S3JobEntry struct {
	Keys          []string
	OldestModTime time.Time
}

// s3Object is an internal DTO used by groupByJobID.
type s3Object struct {
	key     string
	modTime time.Time
}

// groupByJobID groups S3 objects by their first path segment (the job ID).
// Objects whose key has no "/" or starts with "/" are ignored (not job files).
// OldestModTime tracks the earliest LastModified across all objects for a job.
func groupByJobID(objects []s3Object) map[string]S3JobEntry {
	result := make(map[string]S3JobEntry)
	for _, obj := range objects {
		idx := strings.IndexByte(obj.key, '/')
		if idx <= 0 {
			continue
		}
		jobID := obj.key[:idx]
		entry := result[jobID]
		entry.Keys = append(entry.Keys, obj.key)
		if entry.OldestModTime.IsZero() || obj.modTime.Before(entry.OldestModTime) {
			entry.OldestModTime = obj.modTime
		}
		result[jobID] = entry
	}
	return result
}

// NewS3Client builds a standard S3 client from the provided config.
// The BaseEndpoint field makes it compatible with any S3-compatible provider.
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

	s3Client := s3.New(opts)

	return &S3Client{
		s3:       s3Client,
		uploader: manager.NewUploader(s3Client),
		bucket:   cfg.Bucket,
		encKey:   encKey,
	}, nil
}

// s3Span starts a span for an S3 operation and returns a closer that records
// any error on the span before ending it.
func s3Span(ctx context.Context, operation, key string) (context.Context, func(error)) {
	ctx, span := otel.Tracer("gatewai/gateway").Start(ctx, "gateway.s3."+operation,
		trace.WithAttributes(
			attribute.String("s3.operation", operation),
			attribute.String("s3.key", key),
		))
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}

// Upload stores a file stream as objectKey in the configured bucket.
// If encryption is enabled the stream is encrypted before upload.
// Uses multipart upload to support non-seekable streams (io.Pipe).
func (c *S3Client) Upload(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) (err error) {
	ctx, endSpan := s3Span(ctx, "upload", objectKey)
	defer func() { endSpan(err) }()

	start := time.Now()
	body := crypto.Encrypt(c.encKey, reader)
	defer body.Close()

	_, err = c.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(objectKey),
		Body:        body,
		ContentType: aws.String(contentType),
	})
	metrics.ObserveWithExemplar(ctx, metrics.S3OperationDuration.WithLabelValues("upload"), time.Since(start).Seconds())
	if err != nil {
		metrics.S3ErrorsTotal.WithLabelValues("upload").Inc()
		return fmt.Errorf("uploading %q to S3 bucket %q: %w", objectKey, c.bucket, err)
	}
	return nil
}

// GetObject downloads an object and returns its content as bytes.
// If encryption is enabled the data is decrypted before being returned.
func (c *S3Client) GetObject(ctx context.Context, objectKey string) (_ []byte, err error) {
	ctx, endSpan := s3Span(ctx, "get", objectKey)
	defer func() { endSpan(err) }()

	start := time.Now()
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(objectKey),
	})
	metrics.ObserveWithExemplar(ctx, metrics.S3OperationDuration.WithLabelValues("get"), time.Since(start).Seconds())
	if err != nil {
		metrics.S3ErrorsTotal.WithLabelValues("get").Inc()
		return nil, fmt.Errorf("getting S3 object %q: %w", objectKey, err)
	}

	body := crypto.Decrypt(c.encKey, out.Body)
	defer body.Close()

	data, readErr := io.ReadAll(body)
	if readErr != nil {
		err = readErr
		return nil, fmt.Errorf("reading S3 object %q: %w", objectKey, readErr)
	}
	return data, nil
}

// DeleteObject removes an object from the configured bucket.
func (c *S3Client) DeleteObject(ctx context.Context, objectKey string) (err error) {
	ctx, endSpan := s3Span(ctx, "delete", objectKey)
	defer func() { endSpan(err) }()

	start := time.Now()
	_, err = c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(objectKey),
	})
	metrics.ObserveWithExemplar(ctx, metrics.S3OperationDuration.WithLabelValues("delete"), time.Since(start).Seconds())
	if err != nil {
		metrics.S3ErrorsTotal.WithLabelValues("delete").Inc()
		return fmt.Errorf("deleting S3 object %q: %w", objectKey, err)
	}
	return nil
}

// ListJobObjects lists all objects in the bucket and groups them by job ID
// (first path segment of the key). Returns a map of jobID → S3JobEntry.
// Objects with no "/" in their key are ignored.
func (c *S3Client) ListJobObjects(ctx context.Context) (map[string]S3JobEntry, error) {
	start := time.Now()
	paginator := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
	})

	var objects []s3Object
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			metrics.ObserveWithExemplar(ctx, metrics.S3OperationDuration.WithLabelValues("list"), time.Since(start).Seconds())
			metrics.S3ErrorsTotal.WithLabelValues("list").Inc()
			return nil, fmt.Errorf("listing S3 objects: %w", err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil || obj.LastModified == nil {
				continue
			}
			objects = append(objects, s3Object{
				key:     aws.ToString(obj.Key),
				modTime: *obj.LastModified,
			})
		}
	}
	metrics.S3OperationDuration.WithLabelValues("list").Observe(time.Since(start).Seconds())

	return groupByJobID(objects), nil
}
