package attachment

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/digitaldrywood/detent/internal/artifact"
)

const DeletionBatchSize = 1000
const DeletionReceiptLimit = 100

type DeletionFailure struct {
	Key       string `json:"key"`
	VersionID string `json:"version_id,omitempty"`
	Code      string `json:"code"`
}

type DeletionReceipt struct {
	Complete  bool              `json:"complete"`
	Truncated bool              `json:"truncated"`
	Remaining []DeletionFailure `json:"remaining"`
}

type OrganizationEraser interface {
	EraseOrganization(context.Context, string) (DeletionReceipt, error)
}

func (s *spaces) ArchiveVersioning(ctx context.Context) (bool, error) {
	result, err := s.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(s.config.Bucket)})
	if err != nil {
		return false, artifact.ErrStorage
	}
	return result.Status != "", nil
}

func (s *spaces) EraseOrganization(ctx context.Context, organization string) (DeletionReceipt, error) {
	logical, err := OrganizationPrefix(organization)
	if err != nil {
		return DeletionReceipt{}, err
	}
	prefix := s.prefix + logical
	receipt := DeletionReceipt{}
	failure := func(key, version, code string) {
		if len(receipt.Remaining) < DeletionReceiptLimit {
			receipt.Remaining = append(receipt.Remaining, DeletionFailure{Key: key, VersionID: version, Code: code})
		} else {
			receipt.Truncated = true
			receipt.Remaining[DeletionReceiptLimit-1] = DeletionFailure{Key: prefix, Code: "additional_failures"}
		}
	}
	versioning, err := s.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(s.config.Bucket)})
	if err != nil {
		failure(prefix, "", "versioning_unavailable")
		return receipt, artifact.ErrStorage
	}
	visited := 0
	remove := func(key, version string) error {
		if !strings.HasPrefix(key, prefix) {
			failure(prefix, "", "invalid_listing")
			return ErrInvalid
		}
		if visited >= DeletionBatchSize {
			receipt.Truncated = true
			failure(prefix, "", "scan_pending")
			return artifact.ErrStorage
		}
		visited++
		input := &s3.DeleteObjectInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(key)}
		if version != "" {
			input.VersionId = aws.String(version)
		}
		if _, err := s.client.DeleteObject(ctx, input); err != nil {
			failure(key, version, "delete_refused")
		}
		return nil
	}
	if versioning.Status != "" {
		pages := s3.NewListObjectVersionsPaginator(s.client, &s3.ListObjectVersionsInput{Bucket: aws.String(s.config.Bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(100)})
		for pageNumber := 0; pages.HasMorePages(); pageNumber++ {
			if pageNumber >= 10 {
				receipt.Truncated = true
				failure(prefix, "", "scan_pending")
				return receipt, artifact.ErrStorage
			}
			page, err := pages.NextPage(ctx)
			if err != nil {
				failure(prefix, "", "list_versions_unavailable")
				return receipt, artifact.ErrStorage
			}
			for _, object := range page.Versions {
				if aws.ToString(object.VersionId) == "" {
					failure(prefix, "", "invalid_listing")
					return receipt, ErrInvalid
				}
				if err := remove(aws.ToString(object.Key), aws.ToString(object.VersionId)); err != nil {
					return receipt, err
				}
			}
			for _, object := range page.DeleteMarkers {
				if aws.ToString(object.VersionId) == "" {
					failure(prefix, "", "invalid_listing")
					return receipt, ErrInvalid
				}
				if err := remove(aws.ToString(object.Key), aws.ToString(object.VersionId)); err != nil {
					return receipt, err
				}
			}
		}
	} else {
		pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.config.Bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(100)})
		for pageNumber := 0; pages.HasMorePages(); pageNumber++ {
			if pageNumber >= 10 {
				receipt.Truncated = true
				failure(prefix, "", "scan_pending")
				return receipt, artifact.ErrStorage
			}
			page, err := pages.NextPage(ctx)
			if err != nil {
				failure(prefix, "", "list_objects_unavailable")
				return receipt, artifact.ErrStorage
			}
			for _, object := range page.Contents {
				if err := remove(aws.ToString(object.Key), ""); err != nil {
					return receipt, err
				}
			}
		}
	}
	receipt.Complete = len(receipt.Remaining) == 0 && !receipt.Truncated
	if !receipt.Complete {
		return receipt, artifact.ErrStorage
	}
	return receipt, nil
}
