package eventstore

import (
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
