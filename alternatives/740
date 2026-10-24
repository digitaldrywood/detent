package cli

import (
	"context"
	"errors"
	"testing"

	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestRunnerIntakeTokenSourceUsesCurrentInstanceCredential(t *testing.T) {
	t.Parallel()
	current := "first-instance-credential"
	source := newRunnerIntakeTokenSource(func() string { return current }, func(context.Context) (string, error) { current = "refreshed-instance-credential"; return current, nil })
	for _, token := range []string{"first-instance-credential", "reloaded-instance-credential"} {
		current = token
		if got, err := source.Token(t.Context()); err != nil || got != token {
			t.Fatalf("token = %q, error = %v", got, err)
		}
	}
	refresher := source.(githubconnector.RefreshableTokenSource)
	if got, err := refresher.RefreshToken(t.Context()); err != nil || got != "refreshed-instance-credential" {
		t.Fatalf("refresh = %q, error = %v", got, err)
	}
	current = ""
	if _, err := source.Token(t.Context()); !errors.Is(err, githubconnector.ErrMissingToken) {
		t.Fatalf("missing credential = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := source.Token(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled token lookup = %v", err)
	}
}
