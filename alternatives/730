package cli

import (
	"context"
	"strings"

	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

type runnerIntakeTokenSource struct {
	current func() string
	refresh githubconnector.TokenRefreshFunc
}

func newRunnerIntakeTokenSource(current func() string, refresh githubconnector.TokenRefreshFunc) githubconnector.TokenSource {
	return runnerIntakeTokenSource{current: current, refresh: refresh}
}

func (s runnerIntakeTokenSource) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	token := strings.TrimSpace(s.current())
	if token == "" {
		return "", githubconnector.ErrMissingToken
	}
	return token, nil
}

func (s runnerIntakeTokenSource) RefreshToken(ctx context.Context) (string, error) {
	if s.refresh == nil {
		return "", githubconnector.ErrMissingToken
	}
	return s.refresh(ctx)
}
