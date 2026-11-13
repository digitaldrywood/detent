package eventstore

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

type failingContentReader struct {
	attachment.DetailStore
	failure   error
	reference attachment.ArchiveReference
}

func (s failingContentReader) StoreDetail(context.Context, string, []byte) (attachment.ArchiveReference, error) {
	return s.reference, nil
}

func (s failingContentReader) ReadDetail(context.Context, string, attachment.ArchiveReference) ([]byte, error) {
	return nil, s.failure
}

func TestContentReadPreservesStorageFailureClass(t *testing.T) {
	digest := artifact.Digest([]byte(`{}`))
	key, err := attachment.ArchiveKey("org_example", digest)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		failure   error
		integrity bool
	}{
		{"backend", artifact.ErrStorage, false},
		{"missing", artifact.ErrMissing, true},
		{"not exist", fs.ErrNotExist, true},
		{"integrity", artifact.ErrIntegrity, true},
		{"cancelled", context.Canceled, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := NewContent(failingContentReader{failure: test.failure, reference: Reference(key, digest, 10, 2)})
			_, err := content.CollaborationVersion(t.Context(), CollaborationVersion{OrganizationID: "org_example", PayloadObjectKey: key, PayloadSHA256: digest, PayloadBytes: 10, PayloadContentBytes: 2})
			if !errors.Is(err, test.failure) || errors.Is(err, artifact.ErrIntegrity) != test.integrity {
				t.Fatalf("read failure=%v, want %v integrity=%t", err, test.failure, test.integrity)
			}
			_, err = content.PreparePayload(t.Context(), "org_example", []byte(`{}`))
			if !errors.Is(err, test.failure) || errors.Is(err, artifact.ErrIntegrity) != test.integrity {
				t.Fatalf("verification failure=%v, want %v integrity=%t", err, test.failure, test.integrity)
			}
		})
	}
}

type versionContentStore struct {
	attachment.DetailStore
	raw []byte
}

func (s *versionContentStore) StoreDetail(_ context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	s.raw = bytes.Clone(raw)
	digest := artifact.Digest(raw)
	key, err := attachment.ArchiveKey(organization, digest)
	return Reference(key, digest, int64(len(raw)), int64(len(raw))), err
}

func (s *versionContentStore) ReadDetail(context.Context, string, attachment.ArchiveReference) ([]byte, error) {
	return bytes.Clone(s.raw), nil
}

func TestCollaborationVersionPreservesHistoricalBytes(t *testing.T) {
	for _, raw := range []string{"  { \"body\": \"<value>\" }\n", "{\"body\":\"雪\",\"value\":1.00}"} {
		t.Run(raw, func(t *testing.T) {
			content := NewContent(&versionContentStore{})
			row, err := content.PrepareCollaborationVersion(t.Context(), CollaborationVersion{OrganizationID: "org_example"}, []byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			stored, err := content.CollaborationVersion(t.Context(), row)
			if err != nil || string(stored) != raw || row.PayloadContentBytes != int64(len(raw)) {
				t.Fatalf("historical bytes changed: got %q length=%d error=%v, want %q", stored, row.PayloadContentBytes, err, raw)
			}
		})
	}
}
