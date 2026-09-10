package githublocal

import (
	"context"
	"errors"
	"testing"
)

type mergeQueuePolicyBackend struct {
	githubBackend
	calls int
	err   error
}

func (b *mergeQueuePolicyBackend) RefreshMergeQueuePolicy(context.Context) error {
	b.calls++
	return b.err
}

func TestRefreshMergeQueuePolicy(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "provider failure", err: errors.New("rules unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend := &mergeQueuePolicyBackend{err: tt.err}
			c := &Connector{github: backend}
			if err := c.RefreshMergeQueuePolicy(t.Context()); !errors.Is(err, tt.err) {
				t.Fatalf("error=%v", err)
			}
			if backend.calls != 1 {
				t.Fatalf("calls=%d", backend.calls)
			}
		})
	}
	t.Run("backend without refresh capability", func(t *testing.T) {
		c := &Connector{github: &recordingGitHubBackend{}}
		if err := c.RefreshMergeQueuePolicy(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
