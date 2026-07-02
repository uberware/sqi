// SPDX-License-Identifier: AGPL-3.0-or-later

// Command s3sync is a TEST-ONLY sync helper for the S3 staging integration test.
// It copies a single object between the local filesystem and an S3-compatible
// endpoint, in whichever direction the src/dest imply:
//
//	s3sync <src> <dest>
//
// Exactly one of src/dest is an s3://bucket/key URI; the other is a local path.
// Endpoint and credentials come from the environment (AWS_ENDPOINT_URL,
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION), mirroring how an operator
// configures a real aws/mc/rclone sync command. This helper is never built into
// sqi's shipped binaries — it exists only so the integration test can run a real
// S3 round-trip against an in-process fake S3 with no external tools.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: s3sync <src> <dest>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "s3sync:", err)
		os.Exit(1)
	}
}

func run(src, dst string) error {
	client, err := newClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch {
	case isS3(src) && !isS3(dst):
		bucket, key := parseS3(src)
		return client.FGetObject(ctx, bucket, key, dst, minio.GetObjectOptions{})
	case !isS3(src) && isS3(dst):
		bucket, key := parseS3(dst)
		_, err := client.FPutObject(ctx, bucket, key, src, minio.PutObjectOptions{})
		return err
	default:
		return fmt.Errorf("exactly one of src/dest must be an s3:// URI (src=%q dest=%q)", src, dst)
	}
}

func isS3(s string) bool { return strings.HasPrefix(s, "s3://") }

func parseS3(uri string) (bucket, key string) {
	rest := strings.TrimPrefix(uri, "s3://")
	if b, k, ok := strings.Cut(rest, "/"); ok {
		return b, k
	}
	return rest, ""
}

func newClient() (*minio.Client, error) {
	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	if endpoint == "" {
		return nil, errors.New("AWS_ENDPOINT_URL is not set")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse AWS_ENDPOINT_URL %q: %w", endpoint, err)
	}
	return minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), ""),
		Secure:       u.Scheme == "https",
		Region:       os.Getenv("AWS_REGION"),
		BucketLookup: minio.BucketLookupPath,
	})
}
