// Package s3store stores raw API events in an S3-compatible object store
// (Backblaze B2, MinIO, AWS S3) instead of the Postgres `events` table.
//
// # Why an object store
//
// The `events` table grows by millions of rows a day and was the only thing
// actually filling the database host's disk. Users, API keys, sessions and
// the pre-aggregated rollup tables are tiny and stay in Postgres; the raw
// stream — the part that grows without bound — moves here.
//
// # Layout
//
// One object per (hour bucket, flush), keyed so that a time range is a
// prefix listing:
//
//	events/dt=2026-11-21/hour=14/0000000000123456.ndjson.gz
//
// Objects are gzipped newline-delimited JSON with short field names, because
// the object count and the bytes are what the operator pays for.
//
// # What an object store cannot do
//
// There is no SQL here. Every read is a listing plus a linear decode, so:
//
//   - reads walk hour buckets newest-first and stop as soon as they have
//     enough rows (the dashboard always pages newest-first with a small
//     limit, so this is a handful of GETs in practice);
//   - the rollup worker has to stream an hour back and aggregate in Go;
//   - a single event is located by deriving its hour from the timestamp
//     embedded in its id, so `GET /v1/metrics/event/:id` stays one listing.
//
// # No bucket prefix scan, ever
//
// Nothing in this package calls ListObjects without a prefix. B2 bills per
// list operation and an unbucketed scan of a busy bucket is both slow and
// expensive.
package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ObjectStore is the narrow slice of an S3 API this package needs. It exists
// so the store logic — key layout, buffering, filtering, retention — can be
// tested against an in-memory implementation instead of a live bucket.
type ObjectStore interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, keys []string) error
	List(ctx context.Context, prefix string, fn func(key string) error) error
}

// connectTimeout bounds the initial handshake and bucket check.
const connectTimeout = 15 * time.Second

// minioStore is the production ObjectStore, backed by minio-go's
// S3-compatible client. minio-go is used rather than the AWS SDK because it
// is much smaller and treats non-AWS S3 implementations as first-class,
// which is exactly what B2 is.
type minioStore struct {
	client *minio.Client
	bucket string
}

// NewMinioStore opens a client and verifies the bucket is actually reachable.
// A connection is never returned together with an error.
//
// This deliberately does not fall back to Postgres when the bucket is
// unreachable. APP_EVENT_STORE=s3 is an instruction about where events go;
// silently writing them into the database instead is how the disk filled up
// in the first place, and it would do so while reporting healthy startup.
func NewMinioStore(ctx context.Context, endpoint, region, bucket, accessKeyID, secretKey string, pathStyle bool) (ObjectStore, error) {
	if endpoint == "" || bucket == "" {
		return nil, errors.New("s3store: endpoint and bucket are required")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("s3store: parse endpoint %q: %w", endpoint, err)
	}
	if u.Host == "" {
		// Accept a bare host ("s3.us-east-005.backblazeb2.com").
		u, err = url.Parse("https://" + endpoint)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("s3store: endpoint %q is not a host or URL", endpoint)
		}
	}

	lookup := minio.BucketLookupAuto
	if pathStyle {
		lookup = minio.BucketLookupPath
	}

	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKeyID, secretKey, ""),
		Secure:       u.Scheme != "http",
		Region:       region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("s3store: client for %s: %w", u.Host, err)
	}

	checkCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	exists, err := client.BucketExists(checkCtx, bucket)
	if err != nil {
		return nil, fmt.Errorf("s3store: bucket %q at %s: %w", bucket, u.Host, err)
	}
	if !exists {
		return nil, fmt.Errorf("s3store: bucket %q does not exist at %s", bucket, u.Host)
	}

	return &minioStore{client: client, bucket: bucket}, nil
}

// Put writes one object. The content type says what it is rather than
// letting the store guess from the extension.
func (m *minioStore) Put(ctx context.Context, key string, data []byte) error {
	_, err := m.client.PutObject(ctx, m.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/x-ndjson", ContentEncoding: "gzip"})
	if err != nil {
		return fmt.Errorf("s3store: put %s: %w", key, err)
	}
	return nil
}

// Get reads one object whole. Object sizes here are bounded by the writer's
// flush threshold (tens of thousands of events), so streaming into a caller
// would add complexity for no benefit.
//
// A key that is simply gone reports ErrObjectNotFound rather than a generic
// error, because the retention pass deletes objects while reads may be in
// flight and that race must not fail a dashboard request.
func (m *minioStore) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := m.client.GetObject(ctx, m.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("s3store: get %s: %w", key, err)
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		// GetObject is lazy: for S3-compatible servers the status only
		// surfaces when the body is read.
		if isNotFound(err) || errors.Is(err, io.EOF) && len(data) == 0 {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("s3store: read %s: %w", key, err)
	}
	return data, nil
}

// isNotFound recognises a missing-key response across the shapes
// S3-compatible servers use.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrObjectNotFound) {
		return true
	}
	resp := minio.ToErrorResponse(err)
	if resp.StatusCode == http.StatusNotFound {
		return true
	}
	switch resp.Code {
	case "NoSuchKey", "NoSuchObject", "NotFound", "NoSuchBucket":
		return true
	}
	return false
}

// Delete removes keys, reporting how many succeeded. A missing key is not an
// error: retention races nothing, but a concurrent pass may have already
// taken it.
func (m *minioStore) Delete(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	objects := make(chan minio.ObjectInfo, len(keys))
	for _, k := range keys {
		objects <- minio.ObjectInfo{Key: k}
	}
	close(objects)

	var failed int
	var lastErr error
	for res := range m.client.RemoveObjects(ctx, m.bucket, objects, minio.RemoveObjectsOptions{}) {
		if res.Err != nil {
			failed++
			lastErr = res.Err
		}
	}
	if failed > 0 {
		return fmt.Errorf("s3store: %d of %d deletes failed: %w", failed, len(keys), lastErr)
	}
	return nil
}

// List calls fn for every object under prefix. Iteration stops at the first
// error fn returns.
func (m *minioStore) List(ctx context.Context, prefix string, fn func(key string) error) error {
	for obj := range m.client.ListObjects(ctx, m.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return fmt.Errorf("s3store: list %s: %w", prefix, obj.Err)
		}
		if strings.HasSuffix(obj.Key, "/") {
			continue
		}
		if err := fn(obj.Key); err != nil {
			return err
		}
	}
	return nil
}

// mustPrefix guards the invariant the package doc promises: no listing
// without a prefix. An empty prefix would enumerate the whole bucket.
func mustPrefix(prefix string) error {
	if strings.TrimSpace(prefix) == "" {
		return errors.New("s3store: refusing to list the whole bucket (empty prefix)")
	}
	return nil
}

// logf is a seam so the store's chatter goes through the same logger as the
// rest of the service.
var logf = log.Printf
