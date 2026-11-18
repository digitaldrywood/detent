package hubrecords

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

type failingDetailStore struct{ err error }

func (f failingDetailStore) StoreDetail(context.Context, string, []byte) (attachment.ArchiveReference, error) {
	return attachment.ArchiveReference{}, f.err
}

func (f failingDetailStore) ReadDetail(context.Context, string, attachment.ArchiveReference) ([]byte, error) {
	return nil, f.err
}

func TestRunEventDetailStoreReadClassifiesFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		err         error
		wantMissing bool
		wantStorage bool
	}{
		{"missing object", artifact.ErrMissing, true, false},
		{"untyped store failure", errors.New("connection reset"), false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := RunEventDetailStore{DetailStore: failingDetailStore{err: test.err}}.ReadDetail(t.Context(), "org_example", attachment.ArchiveReference{})
			if errors.Is(err, artifact.ErrMissing) != test.wantMissing || errors.Is(err, artifact.ErrStorage) != test.wantStorage {
				t.Fatalf("ReadDetail error = %v, want missing=%t storage=%t", err, test.wantMissing, test.wantStorage)
			}
		})
	}
}
