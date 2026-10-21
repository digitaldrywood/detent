package attachment

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
)

const ArchiveRetention = "organization"
const ArchiveReadLifetime = 5 * time.Minute
const MaxArchiveBytes = 16 << 20

type ArchiveReference struct {
	BindingID        string `json:"binding_id,omitempty"`
	CustomerOwned    bool   `json:"customer_owned,omitempty"`
	Key              string `json:"key"`
	SHA256           string `json:"sha256"`
	CompressedSize   int64  `json:"compressed_size"`
	UncompressedSize int64  `json:"uncompressed_size"`
	Codec            string `json:"codec"`
	SchemaVersion    int    `json:"schema_version"`
	Kind             string `json:"kind"`
	Retention        string `json:"retention"`
}

type ArchiveRead struct {
	ArchiveReference
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (ArchiveRead) LogValue() slog.Value {
	return slog.StringValue("archive read")
}

type ArchiveStorage interface {
	Storage
	PutArchive(context.Context, ArchiveReference, []byte) error
	PresignArchive(context.Context, ArchiveReference) (ArchiveRead, error)
	ConfigureArchiveReads(context.Context, []string) error
	ArchiveVersioning(context.Context) (bool, error)
}

func ArchiveKey(organization, digest string) (string, error) {
	if !validOrganization(organization) || !artifact.ValidHash(digest, 64) {
		return "", ErrInvalid
	}
	return "orgs/" + organization + "/blobs/sha256/" + digest[:2] + "/" + digest, nil
}

func (r ArchiveReference) Validate(organization string) error {
	key, err := ArchiveKey(organization, r.SHA256)
	if (r.BindingID != "" && !validOrganization(r.BindingID)) || (r.CustomerOwned && r.BindingID == "") || err != nil || key != r.Key || r.CompressedSize <= 0 || r.CompressedSize > MaxBytes || r.UncompressedSize <= 0 || r.UncompressedSize > MaxArchiveBytes || r.Codec != "gzip" || r.SchemaVersion < 1 || r.Kind != "archive" || r.Retention != ArchiveRetention {
		return ErrInvalid
	}
	return nil
}

func PrepareArchive(organization string, schema int, reader io.Reader) (ArchiveReference, []byte, error) {
	if !validOrganization(organization) || schema < 1 {
		return ArchiveReference{}, nil, ErrInvalid
	}
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, 6)
	if err != nil {
		return ArchiveReference{}, nil, ErrInvalid
	}
	size, err := io.Copy(writer, io.LimitReader(reader, MaxArchiveBytes+1))
	if err = errors.Join(err, writer.Close()); err != nil {
		return ArchiveReference{}, nil, artifact.ErrStorage
	}
	if size > MaxArchiveBytes {
		return ArchiveReference{}, nil, ErrTooLarge
	}
	if size == 0 {
		return ArchiveReference{}, nil, ErrInvalid
	}
	data := compressed.Bytes()
	digest := artifact.Digest(data)
	key, err := ArchiveKey(organization, digest)
	if err != nil {
		return ArchiveReference{}, nil, err
	}
	ref := ArchiveReference{Key: key, SHA256: digest, CompressedSize: int64(len(data)), UncompressedSize: size, Codec: "gzip", SchemaVersion: schema, Kind: "archive", Retention: ArchiveRetention}
	return ref, data, ref.Validate(organization)
}

func StoreArchive(ctx context.Context, storage ArchiveStorage, organization string, ref ArchiveReference, data []byte) error {
	if ref.Validate(organization) != nil || int64(len(data)) != ref.CompressedSize || artifact.Digest(data) != ref.SHA256 {
		return ErrInvalid
	}
	if archiveStored(ctx, storage, ref) {
		return nil
	}
	if err := storage.PutArchive(ctx, ref, data); err != nil {
		return artifact.ErrStorage
	}
	if !archiveStored(ctx, storage, ref) {
		return artifact.ErrIntegrity
	}
	return nil
}

func archiveStored(ctx context.Context, storage Storage, ref ArchiveReference) bool {
	body, err := storage.Open(ctx, ref.Key)
	if err != nil {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(body, ref.CompressedSize+1))
	err = errors.Join(err, body.Close())
	return err == nil && int64(len(data)) == ref.CompressedSize && artifact.Digest(data) == ref.SHA256
}
