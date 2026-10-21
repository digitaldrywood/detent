package attachment

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/digitaldrywood/detent/internal/artifact"
)

func (s *spaces) PutArchive(ctx context.Context, ref ArchiveReference, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(ref.Key), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String("application/gzip")})
	if err != nil {
		return artifact.ErrStorage
	}
	return nil
}

func (s *spaces) PresignArchive(ctx context.Context, ref ArchiveReference) (string, error) {
	result, err := s3.NewPresignClient(s.client).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(ref.Key)}, func(options *s3.PresignOptions) {
		options.Expires = ArchiveReadLifetime
	})
	if err != nil {
		return "", artifact.ErrStorage
	}
	return result.URL, nil
}

func (s *spaces) ConfigureArchiveReads(ctx context.Context, origins []string) error {
	if len(origins) == 0 {
		return ErrInvalid
	}
	for _, origin := range origins {
		if !artifact.ValidOrigin(origin) {
			return ErrInvalid
		}
	}
	lifecycle, err := s.client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(s.config.Bucket)})
	if err != nil {
		var api smithy.APIError
		if !errors.As(err, &api) || api.ErrorCode() != "NoSuchLifecycleConfiguration" {
			return artifact.ErrStorage
		}
	} else {
		for _, rule := range lifecycle.Rules {
			prefix := aws.ToString(rule.Prefix)
			if rule.Filter != nil {
				prefix = aws.ToString(rule.Filter.Prefix)
				if rule.Filter.And != nil {
					prefix = aws.ToString(rule.Filter.And.Prefix)
				}
			}
			if rule.Status == types.ExpirationStatusEnabled && (rule.Expiration != nil || rule.NoncurrentVersionExpiration != nil) && archivePrefixOverlaps(prefix) {
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
