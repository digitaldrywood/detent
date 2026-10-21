package attachment

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/digitaldrywood/detent/internal/artifact"
)

func (s *spaces) PutArchive(ctx context.Context, ref ArchiveReference, data []byte) error {
	if !validArchiveKey(ref) || int64(len(data)) != ref.CompressedSize || artifact.Digest(data) != ref.SHA256 {
		return ErrInvalid
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(s.prefix + ref.Key), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String("application/gzip"), CacheControl: aws.String("private, no-store")})
	if err != nil {
		return artifact.ErrStorage
	}
	return nil
}

func (s *spaces) PresignArchive(ctx context.Context, ref ArchiveReference) (ArchiveRead, error) {
	if !validArchiveKey(ref) {
		return ArchiveRead{}, ErrInvalid
	}
	result, err := s3.NewPresignClient(s.client).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(s.prefix + ref.Key)}, func(options *s3.PresignOptions) {
		options.Expires = ArchiveReadLifetime
	})
	if err != nil {
		return ArchiveRead{}, artifact.ErrStorage
	}
	target, err := url.Parse(result.URL)
	if err != nil {
		return ArchiveRead{}, artifact.ErrStorage
	}
	signedAt, err := time.Parse("20060102T150405Z", target.Query().Get("X-Amz-Date"))
	if err != nil {
		return ArchiveRead{}, artifact.ErrStorage
	}
	return ArchiveRead{ArchiveReference: ref, URL: result.URL, ExpiresAt: signedAt.Add(ArchiveReadLifetime)}, nil
}

func validArchiveKey(ref ArchiveReference) bool {
	parts := strings.Split(ref.Key, "/")
	return len(parts) == 6 && ref.Validate(parts[1]) == nil
}

func (s *spaces) ConfigureArchiveReads(ctx context.Context, origins []string) error {
	if len(origins) == 0 {
		return ErrInvalid
	}
	for _, origin := range origins {
		if !artifact.ValidOrigin(origin) || strings.Contains(origin, "*") {
			return ErrInvalid
		}
	}
	var lifecycle archiveLifecycle
	_, err := s.client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(s.config.Bucket)}, func(options *s3.Options) {
		options.HTTPClient = archiveLifecycleHTTP{client: s.http, configuration: &lifecycle}
	})
	if err != nil {
		var api smithy.APIError
		if !errors.As(err, &api) || api.ErrorCode() != "NoSuchLifecycleConfiguration" {
			return artifact.ErrStorage
		}
	} else {
		for _, rule := range lifecycle.Rules {
			prefix := rule.Prefix
			if rule.Filter != nil {
				prefix = rule.Filter.Prefix
				if rule.Filter.And != nil {
					prefix = rule.Filter.And.Prefix
				}
			}
			if rule.Status == "Enabled" && (rule.Expiration != nil || rule.NoncurrentVersionExpiration != nil) && (strings.HasPrefix(s.prefix+"orgs/", prefix) || strings.HasPrefix(prefix, s.prefix) && archivePrefixOverlaps(strings.TrimPrefix(prefix, s.prefix))) {
				return artifact.ErrUnsupported
			}
		}
	}
	_, err = s.client.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: aws.String(s.config.Bucket), CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{{AllowedOrigins: origins, AllowedMethods: []string{"GET", "HEAD"}, ExposeHeaders: []string{"Content-Length", "Content-Type", "ETag"}}}}})
	if err != nil {
		return artifact.ErrStorage
	}
	return nil
}

func archivePrefixOverlaps(prefix string) bool {
	if strings.HasPrefix("orgs/", prefix) {
		return true
	}
	if !strings.HasPrefix(prefix, "orgs/") {
		return false
	}
	_, tail, found := strings.Cut(strings.TrimPrefix(prefix, "orgs/"), "/")
	return !found || strings.HasPrefix("blobs/sha256/", tail) || strings.HasPrefix(tail, "blobs/sha256/")
}

type archiveLifecycle struct {
	Rules []struct {
		Prefix                      string
		Status                      string
		Expiration                  *struct{}
		NoncurrentVersionExpiration *struct{}
		Filter                      *struct {
			Prefix string
			And    *struct{ Prefix string }
		}
	} `xml:"Rule"`
}

type archiveLifecycleHTTP struct {
	client        *http.Client
	configuration *archiveLifecycle
}

func (c archiveLifecycleHTTP) Do(request *http.Request) (*http.Response, error) {
	response, err := c.client.Do(request)
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return response, err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	err = errors.Join(err, response.Body.Close())
	if err != nil || len(data) > 1<<20 || xml.Unmarshal(data, c.configuration) != nil {
		return nil, artifact.ErrStorage
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	return response, nil
}
