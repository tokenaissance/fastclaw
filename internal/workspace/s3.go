package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 stores objects in any S3-compatible bucket (AWS S3, MinIO, Cloudflare
// R2, Backblaze B2, ...). Works by key prefix: agent foo's file bar.pdf is
// at s3://<bucket>/<prefix>/foo/bar.pdf.
//
// Use NewS3 to build one from a config block rather than constructing the
// struct directly — the minio client needs specific endpoint parsing.
type S3 struct {
	client *minio.Client
	bucket string
	prefix string // prepended to every key; can be "" for bucket root
}

// S3Config holds the bits NewS3 needs. Field naming follows the fastclaw.json
// convention so it round-trips through encoding/json cleanly.
type S3Config struct {
	Endpoint  string `json:"endpoint"`            // e.g. "s3.amazonaws.com", "<acct>.r2.cloudflarestorage.com"
	Region    string `json:"region,omitempty"`    // AWS region; "" for R2/MinIO
	Bucket    string `json:"bucket"`              // target bucket
	Prefix    string `json:"prefix,omitempty"`    // key prefix; useful for multi-env share
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	UseSSL    bool   `json:"useSSL"`              // default false — most managed services enforce SSL anyway
}

// NewS3 builds an S3 Store. Returns a wrapped error instead of panicking so
// the gateway can fall back to LocalFS on misconfiguration without crashing.
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("s3 config requires endpoint/bucket/accessKey/secretKey")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	return &S3{
		client: client,
		bucket: cfg.Bucket,
		prefix: strings.Trim(cfg.Prefix, "/"),
	}, nil
}

// key is <prefix>/<agentID>/<path>, always with forward slashes.
func (s *S3) key(agentID, p string) string {
	parts := []string{}
	if s.prefix != "" {
		parts = append(parts, s.prefix)
	}
	parts = append(parts, agentID, path.Clean("/"+p)[1:])
	return strings.Join(parts, "/")
}

func (s *S3) Put(ctx context.Context, agentID, p string, r io.Reader, size int64, contentType string) error {
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(p))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}
	_, err := s.client.PutObject(ctx, s.bucket, s.key(agentID, p), r, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	return err
}

func (s *S3) Get(ctx context.Context, agentID, p string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.key(agentID, p), minio.GetObjectOptions{})
	if err != nil {
		return nil, mapS3Err(err)
	}
	// Minio's GetObject returns a lazy reader — probe with Stat so callers
	// get the NotFound error upfront, not on the first Read.
	if _, statErr := obj.Stat(); statErr != nil {
		obj.Close()
		return nil, mapS3Err(statErr)
	}
	return obj, nil
}

func (s *S3) Stat(ctx context.Context, agentID, p string) (*ObjectInfo, error) {
	info, err := s.client.StatObject(ctx, s.bucket, s.key(agentID, p), minio.StatObjectOptions{})
	if err != nil {
		return nil, mapS3Err(err)
	}
	return &ObjectInfo{
		Path:        p,
		Size:        info.Size,
		ContentType: info.ContentType,
		ModTime:     info.LastModified.UTC(),
	}, nil
}

func (s *S3) List(ctx context.Context, agentID string) ([]ObjectInfo, error) {
	prefix := s.key(agentID, "")
	// PrefixSlash ensures we only match under the agent's subtree.
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	var out []ObjectInfo
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		rel := strings.TrimPrefix(obj.Key, prefix)
		ctype := obj.ContentType
		if ctype == "" {
			ctype = mime.TypeByExtension(filepath.Ext(rel))
		}
		out = append(out, ObjectInfo{
			Path:        rel,
			Size:        obj.Size,
			ContentType: ctype,
			ModTime:     obj.LastModified.UTC(),
		})
	}
	return out, nil
}

func (s *S3) Delete(ctx context.Context, agentID, p string) error {
	err := s.client.RemoveObject(ctx, s.bucket, s.key(agentID, p), minio.RemoveObjectOptions{})
	if err != nil && !isS3NotFound(err) {
		return err
	}
	return nil
}

// SignedURL is the main reason we want S3 for cloud deployments: download
// requests can bypass the gateway entirely. TTL is typically a few minutes;
// the browser uses the URL once and discards it.
func (s *S3) SignedURL(ctx context.Context, agentID, p string, ttl time.Duration) (string, error) {
	reqParams := url.Values{}
	u, err := s.client.PresignedGetObject(ctx, s.bucket, s.key(agentID, p), ttl, reqParams)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// mapS3Err normalises minio's errors to our ErrNotFound so callers can do a
// simple errors.Is check without knowing the SDK.
func mapS3Err(err error) error {
	if err == nil {
		return nil
	}
	if isS3NotFound(err) {
		return ErrNotFound
	}
	return err
}

func isS3NotFound(err error) bool {
	errResp := minio.ToErrorResponse(err)
	if errResp.Code == "NoSuchKey" || errResp.Code == "NoSuchObject" || errResp.Code == "NotFound" {
		return true
	}
	// Some providers wrap it differently.
	return errors.Is(err, ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), "not found")
}
