// Package objstore is the S3 client for the thumbnail cache held in VersityGW.
//
// Thumbnail bytes pass through this process on both the read and the write
// path. The gateway has no published ports (D-010), so a presigned URL handed
// to a browser would not resolve: ThumbnailService answers with bytes, and Get
// and Put are where they are fetched and stored.
//
// PresignPut and PresignGet have no caller in this repository today. They are
// kept because Gahaku holds no credentials of its own and takes a URL for both
// its input and its output, so handing it one is the only way to keep an object
// out of this process — the shape a larger render or a direct-to-gateway upload
// would need.
package objstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// ErrNotConfigured reports that no endpoint was configured, mirroring the
// tika package: one config is loaded whole and unused sections stay empty.
var ErrNotConfigured = errors.New("objstore: no endpoint configured")

// ErrNoSuchKey reports that an object is absent.
//
// It matters more than an ordinary error: the thumbnail read path treats a
// missing object as a cache miss and re-renders, so misreading a 404 as a
// transport failure would turn a recoverable state into a served error.
var ErrNoSuchKey = errors.New("objstore: no such key")

// Config is the client's configuration, separate from the process config so a
// test can point it at a stub server.
type Config struct {
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	// UsePathStyle addresses buckets as a path under one endpoint. VersityGW
	// has no per-bucket virtual host, so this is normally true.
	UsePathStyle bool
	// Timeout bounds one request to the gateway, as in the tika and gahakuclient
	// packages. Without it a gateway that accepts a connection and then stops
	// answering holds a request — and, on the thumbnail path, a render slot —
	// until the caller's own context expires. 0 leaves the client unbounded.
	Timeout time.Duration
}

// Store is a bucket in one S3 gateway.
type Store struct {
	bucket  string
	client  *s3.Client
	presign *s3.PresignClient
}

// New builds a Store. An empty Config.Endpoint is accepted and surfaces as
// ErrNotConfigured on the first call.
func New(cfg Config) (*Store, error) {
	if cfg.Endpoint == "" {
		return &Store{}, nil
	}
	if cfg.Bucket == "" {
		return nil, errors.New("objstore: bucket is required when an endpoint is set")
	}

	client := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: cfg.UsePathStyle,
		Credentials: credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretAccessKey, "",
		),
		HTTPClient: &http.Client{Timeout: cfg.Timeout},
	})
	return &Store{
		bucket:  cfg.Bucket,
		client:  client,
		presign: s3.NewPresignClient(client),
	}, nil
}

// Bucket returns the configured bucket name.
func (s *Store) Bucket() string { return s.bucket }

// PresignPut returns a URL the holder may PUT one object to until it expires.
func (s *Store) PresignPut(
	ctx context.Context,
	key string,
	contentType string,
	expires time.Duration,
) (string, error) {
	if s.client == nil {
		return "", ErrNotConfigured
	}
	in := &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	req, err := s.presign.PresignPutObject(ctx, in, s3.WithPresignExpires(expires))
	if err != nil {
		return "", fmt.Errorf("objstore: presign put %q: %w", key, err)
	}
	return req.URL, nil
}

// PresignGet returns a URL the holder may GET one object from until it
// expires.
func (s *Store) PresignGet(
	ctx context.Context,
	key string,
	expires time.Duration,
) (string, error) {
	if s.client == nil {
		return "", ErrNotConfigured
	}
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expires))
	if err != nil {
		return "", fmt.Errorf("objstore: presign get %q: %w", key, err)
	}
	return req.URL, nil
}

// Get reads one object whole. A missing key is reported as [ErrNoSuchKey].
//
// Whole rather than streamed because every caller is the thumbnail path, which
// needs the length to answer with and the bytes to put in one response message.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	if s.client == nil {
		return nil, ErrNotConfigured
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("objstore: get %q: %w", key, ErrNoSuchKey)
		}
		return nil, fmt.Errorf("objstore: get %q: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()

	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("objstore: read %q: %w", key, err)
	}
	return body, nil
}

// Put stores one object.
func (s *Store) Put(ctx context.Context, key string, body []byte, contentType string) error {
	if s.client == nil {
		return ErrNotConfigured
	}
	in := &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	if _, err := s.client.PutObject(ctx, in); err != nil {
		return fmt.Errorf("objstore: put %q: %w", key, err)
	}
	return nil
}

// isNotFound recognizes an absent object however the gateway spells it.
//
// The typed *types.NoSuchKey alone is not enough: VersityGW answers a HEAD-like
// GET miss with a bare 404 carrying no S3 error code, which deserializes to a
// generic API error rather than the modelled one. Missing that case would make
// the thumbnail path report a failure where it should re-render.
func isNotFound(err error) bool {
	var noKey *types.NoSuchKey
	if errors.As(err, &noKey) {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	var resp *smithyhttp.ResponseError
	if errors.As(err, &resp) && resp.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return false
}

// Delete removes one object. Deleting a key that is not there succeeds, which
// is what makes the invalidation and eviction jobs safe to retry.
func (s *Store) Delete(ctx context.Context, key string) error {
	if s.client == nil {
		return ErrNotConfigured
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var noKey *types.NoSuchKey
		if errors.As(err, &noKey) {
			return nil
		}
		return fmt.Errorf("objstore: delete %q: %w", key, err)
	}
	return nil
}

// Object is one enumerated object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// List walks every object under prefix, calling fn once per page. Returning an
// error from fn stops the walk and is returned unwrapped, so a caller can use a
// sentinel to stop early.
func (s *Store) List(
	ctx context.Context,
	prefix string,
	fn func(context.Context, []Object) error,
) error {
	if s.client == nil {
		return ErrNotConfigured
	}
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("objstore: list %q: %w", prefix, err)
		}
		objects := make([]Object, 0, len(page.Contents))
		for _, obj := range page.Contents {
			objects = append(objects, Object{
				Key:          aws.ToString(obj.Key),
				Size:         aws.ToInt64(obj.Size),
				LastModified: aws.ToTime(obj.LastModified),
			})
		}
		if err := fn(ctx, objects); err != nil {
			return err
		}
	}
	return nil
}
