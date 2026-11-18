package threads

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
)

type unreachableDetails struct{}

func (unreachableDetails) StoreDetail(context.Context, string, []byte) (attachment.ArchiveReference, error) {
	return attachment.ArchiveReference{}, errors.New("detail store reached after cancellation")
}

func (unreachableDetails) ReadDetail(context.Context, string, attachment.ArchiveReference) ([]byte, error) {
	return nil, errors.New("detail store reached after cancellation")
}

func TestThreadPostHydrationReturnsCanceledContext(t *testing.T) {
	service := New(Deps{Details: func() attachment.DetailStore { return unreachableDetails{} }})
	for _, tc := range []struct {
		name  string
		posts []threadstore.PostRecord
	}{{name: "empty"}, {name: "pending", posts: []threadstore.PostRecord{{}}}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := service.HydratePosts(ctx, "org_example", tc.posts); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled body hydration returned success: %v", err)
			}
		})
	}
}
