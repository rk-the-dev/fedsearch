package objstore

import (
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rksurwase/fedsearch/internal/config"
)

// S3 is a store over one bucket and base prefix. Works with AWS S3 and MinIO.
type S3 struct {
	client *s3.Client
	bucket string
	base   string // prefix inside the bucket, no leading/trailing slash
	ctx    context.Context
}

func newS3(root string, c config.S3) (*S3, error) {
	rest := strings.TrimPrefix(root, "s3://")
	bucket, base, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return nil, fmt.Errorf("bad s3 root %q", root)
	}
	scheme := "http"
	if c.UseSSL {
		scheme = "https"
	}
	region := c.Region
	if region == "" {
		region = "us-east-1"
	}
	opts := s3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, ""),
		UsePathStyle: true, // MinIO and most S3-compatible stores
	}
	if c.Endpoint != "" {
		opts.BaseEndpoint = aws.String(scheme + "://" + c.Endpoint)
	}
	return &S3{client: s3.New(opts), bucket: bucket, base: strings.Trim(base, "/")}, nil
}

func (s *S3) key(rel string) string {
	if s.base == "" {
		return rel
	}
	return path.Join(s.base, rel)
}

func (s *S3) rel(full string) string {
	return strings.TrimPrefix(strings.TrimPrefix(full, s.base), "/")
}

func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String(s.key(prefix) + "/"),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			out = append(out, Object{
				Key: s.rel(aws.ToString(o.Key)), Size: aws.ToInt64(o.Size),
				ETag: strings.Trim(aws.ToString(o.ETag), `"`), Modified: aws.ToTime(o.LastModified),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *S3) Dirs(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String(s.key(prefix) + "/"), Delimiter: aws.String("/"),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, cp := range page.CommonPrefixes {
			out = append(out, strings.TrimSuffix(s.rel(aws.ToString(cp.Prefix)), "/"))
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *S3) ReaderAt(ctx context.Context, key string, size int64) (*CountingReaderAt, error) {
	return &CountingReaderAt{r: &rangeReader{ctx: ctx, s: s, key: s.key(key), size: size}}, nil
}

// URI returns an s3:// URI that DuckDB's httpfs can read.
func (s *S3) URI(key string) string { return "s3://" + s.bucket + "/" + s.key(key) }

// rangeReader issues one ranged GET per ReadAt call.
type rangeReader struct {
	ctx  context.Context
	s    *S3
	key  string
	size int64
}

func (r *rangeReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	end := off + int64(len(p)) - 1
	if end >= r.size {
		end = r.size - 1
	}
	out, err := r.s.client.GetObject(r.ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.s.bucket), Key: aws.String(r.key),
		Range: aws.String(fmt.Sprintf("bytes=%d-%d", off, end)),
	})
	if err != nil {
		return 0, err
	}
	defer out.Body.Close()
	n, err := io.ReadFull(out.Body, p[:end-off+1])
	if err == nil && n < len(p) {
		err = io.EOF
	}
	return n, err
}
