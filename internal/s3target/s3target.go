// Package s3target wraps aws-sdk-go-v2 for S3-compatible object stores
// (AWS S3, Backblaze B2, MinIO and the ProxBack S3 simulator).
package s3target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("s3: object not found")

// Config describes an S3-compatible endpoint.
type Config struct {
	Endpoint  string // empty means AWS default endpoints
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	PathStyle bool
}

// Client is a bucket-scoped S3 client.
type Client struct {
	api    *s3.Client
	bucket string
}

// NormalizeEndpoint makes a pasted endpoint usable: trims whitespace and a
// trailing slash, and defaults the scheme to https:// when it is missing
// (users routinely paste "s3.eu-central-003.backblazeb2.com").
func NormalizeEndpoint(endpoint string) string {
	e := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if e == "" {
		return ""
	}
	if !strings.Contains(e, "://") {
		e = "https://" + e
	}
	return e
}

// regionFromEndpoint extracts the signing region embedded in well-known
// endpoint hostnames (Backblaze B2 "s3.<region>.backblazeb2.com", AWS
// "s3.<region>.amazonaws.com"). Empty when the host encodes no region.
func regionFromEndpoint(endpoint string) string {
	host := endpoint
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/:"); i >= 0 {
		host = host[:i]
	}
	for _, suffix := range []string{".backblazeb2.com", ".amazonaws.com"} {
		if !strings.HasSuffix(host, suffix) {
			continue
		}
		core := strings.TrimSuffix(host, suffix)
		if rest, ok := strings.CutPrefix(core, "s3."); ok && rest != "" && !strings.Contains(rest, ".") {
			return rest
		}
	}
	return ""
}

// New builds an S3 client for the given configuration.
func New(_ context.Context, cfg Config) (*Client, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket required")
	}
	cfg.Endpoint = NormalizeEndpoint(cfg.Endpoint)
	region := cfg.Region
	// Providers like Backblaze B2 reject signatures whose scope region does
	// not match the endpoint, so the region embedded in the endpoint hostname
	// is authoritative when present.
	if derived := regionFromEndpoint(cfg.Endpoint); derived != "" {
		region = derived
	}
	if region == "" {
		region = "us-east-1"
	}
	awsCfg := aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		// Many S3-compatible implementations reject the SDK's default
		// flexible checksum trailers, so only send them when required.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		HTTPClient:                 &http.Client{Timeout: 5 * time.Minute},
	}
	api := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(strings.TrimRight(cfg.Endpoint, "/"))
		}
		o.UsePathStyle = cfg.PathStyle
	})
	return &Client{api: api, bucket: cfg.Bucket}, nil
}

// Bucket returns the bucket this client is scoped to.
func (c *Client) Bucket() string { return c.bucket }

// EnsureBucket creates the bucket when it does not exist yet.
func (c *Client) EnsureBucket(ctx context.Context) error {
	_, err := c.api.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &c.bucket})
	if err == nil {
		return nil
	}
	_, err = c.api.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &c.bucket})
	if err == nil {
		return nil
	}
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if errors.As(err, &owned) || errors.As(err, &exists) {
		return nil
	}
	return fmt.Errorf("s3: create bucket %q: %w", c.bucket, err)
}

// Put stores an object.
func (c *Client) Put(ctx context.Context, key string, data []byte) error {
	_, err := c.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &c.bucket,
		Key:           &key,
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	})
	if err != nil {
		return fmt.Errorf("s3: put %q: %w", key, err)
	}
	return nil
}

// Get opens an object for reading. The caller must close the reader.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := c.api.GetObject(ctx, &s3.GetObjectInput{Bucket: &c.bucket, Key: &key})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, fmt.Errorf("s3: get %q: %w", key, err)
	}
	return out.Body, nil
}

// GetBytes reads a whole object into memory.
func (c *Client) GetBytes(ctx context.Context, key string) ([]byte, error) {
	rc, err := c.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("s3: read %q: %w", key, err)
	}
	return b, nil
}

// Head reports whether an object exists and its size.
func (c *Client) Head(ctx context.Context, key string) (size int64, exists bool, err error) {
	out, err := c.api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &c.bucket, Key: &key})
	if err != nil {
		if isNotFound(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("s3: head %q: %w", key, err)
	}
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return size, true, nil
}

// Delete removes an object. Deleting a missing object is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &c.bucket, Key: &key})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("s3: delete %q: %w", key, err)
	}
	return nil
}

// Object is one entry from a listing.
type Object struct {
	Key  string
	Size int64
	// LastModified is the object's server-side timestamp. Not every S3
	// implementation reports it, so treat the zero value as "unknown".
	LastModified time.Time
}

// List returns every object under prefix.
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	var token *string
	for {
		page, err := c.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            &c.bucket,
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("s3: list %q: %w", prefix, err)
		}
		for _, o := range page.Contents {
			if o.Key == nil {
				continue
			}
			var size int64
			if o.Size != nil {
				size = *o.Size
			}
			var modified time.Time
			if o.LastModified != nil {
				modified = o.LastModified.UTC()
			}
			out = append(out, Object{Key: *o.Key, Size: size, LastModified: modified})
		}
		if page.IsTruncated == nil || !*page.IsTruncated || page.NextContinuationToken == nil {
			break
		}
		token = page.NextContinuationToken
	}
	return out, nil
}

// Test performs a put/get/delete round trip on a probe object. It never
// creates the bucket: restricted credentials (e.g. a Backblaze application key
// scoped to one bucket) are not allowed to, and the bucket is expected to
// already exist.
func (c *Client) Test(ctx context.Context) error {
	key := fmt.Sprintf(".proxback-probe/%d", time.Now().UnixNano())
	want := []byte("proxback connectivity probe")
	if err := c.Put(ctx, key, want); err != nil {
		return err
	}
	got, err := c.GetBytes(ctx, key)
	if err != nil {
		_ = c.Delete(ctx, key)
		return err
	}
	if !bytes.Equal(got, want) {
		_ = c.Delete(ctx, key)
		return errors.New("s3: probe object round trip mismatch")
	}
	if err := c.Delete(ctx, key); err != nil {
		return err
	}
	return nil
}

func isNotFound(err error) bool {
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nsb *types.NoSuchBucket
	if errors.As(err, &nsb) {
		return true
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return false
}
