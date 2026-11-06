package attachment

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
)

type DetailStore interface {
	StoreDetail(context.Context, string, []byte) (ArchiveReference, error)
	ReadDetail(context.Context, string, ArchiveReference) ([]byte, error)
}

func ReadArchive(ctx context.Context, storage Storage, organization string, ref ArchiveReference) ([]byte, error) {
	if ref.Validate(organization) != nil {
		return nil, ErrInvalid
	}
	compressed, err := ReadArchiveObject(ctx, storage, organization, ref)
	if err != nil {
		return nil, err
	}
	return decodeArchive(compressed, ref.CompressedSize, ref.UncompressedSize, true)
}

func decodeArchive(compressed []byte, compressedSize, contentLimit int64, exact bool) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, &ArchiveReadError{BytesRead: int64(len(compressed)), ExpectedSize: compressedSize, Err: errors.Join(artifact.ErrIntegrity, err)}
	}
	data, err := io.ReadAll(io.LimitReader(reader, contentLimit+1))
	if err = errors.Join(err, reader.Close()); err != nil || int64(len(data)) > contentLimit || exact && int64(len(data)) != contentLimit {
		return nil, &ArchiveReadError{BytesRead: int64(len(compressed)), ExpectedSize: compressedSize, Err: errors.Join(artifact.ErrIntegrity, err)}
	}
	return data, nil
}

type CanonicalDetailStore interface {
	ReadCanonicalDetail(context.Context, string, string, string) ([]byte, error)
}

func ValidateCanonicalDetailKey(organization, project, key string) error {
	digest := key[strings.LastIndex(key, "/")+1:]
	expected, err := ArchiveKey(organization, digest)
	if err != nil || !validOrganization(project) || key != expected {
		return artifact.ErrIntegrity
	}
	return nil
}

func ReadCanonicalArchive(ctx context.Context, storage Storage, organization, project, key string) ([]byte, error) {
	if err := ValidateCanonicalDetailKey(organization, project, key); err != nil {
		return nil, err
	}
	body, err := storage.Open(ctx, key)
	if err != nil {
		return nil, errors.Join(artifact.ErrIntegrity, err, ctx.Err())
	}
	compressed, err := io.ReadAll(io.LimitReader(body, MaxArchiveBytes+1))
	if err = errors.Join(err, body.Close(), ctx.Err()); err != nil {
		return nil, errors.Join(artifact.ErrIntegrity, err)
	}
	digest := key[strings.LastIndex(key, "/")+1:]
	if len(compressed) == 0 || len(compressed) > MaxArchiveBytes || artifact.Digest(compressed) != digest {
		return nil, artifact.ErrIntegrity
	}
	data, err := decodeArchive(compressed, int64(len(compressed)), MaxArchiveBytes, false)
	if err != nil {
		return nil, err
	}
	ref := ArchiveReference{Key: key, SHA256: digest, CompressedSize: int64(len(compressed)), UncompressedSize: int64(len(data)), Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: ArchiveRetention}
	if ref.Validate(organization) != nil {
		return nil, artifact.ErrIntegrity
	}
	return data, nil
}
