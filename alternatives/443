package cli_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/cli"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/project"
)

func TestExitCodeClassifiesRepresentativeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "success", want: cli.ExitSuccess},
		{name: "context canceled", err: context.Canceled, want: cli.ExitSuccess},
		{name: "general", err: errors.New("boom"), want: cli.ExitGeneral},
		{name: "doctor failed", err: cli.ErrDoctorFailed, want: cli.ExitGeneral},
		{name: "shutdown forced", err: cli.ErrShutdownForced, want: cli.ExitGeneral},
		{name: "shutdown timeout", err: cli.ErrShutdownTimeout, want: cli.ExitGeneral},
		{name: "runtime github auth", err: fmt.Errorf("wrapped: %w", cli.ErrGitHubAuth), want: cli.ExitAuth},
		{name: "connector missing token", err: fmt.Errorf("wrapped: %w", githubconnector.ErrMissingToken), want: cli.ExitAuth},
		{name: "validation", err: fmt.Errorf("wrapped: %w", cli.ErrValidation), want: cli.ExitValidation},
		{name: "invalid output format", err: fmt.Errorf("wrapped: %w", cli.ErrInvalidOutputFormat), want: cli.ExitValidation},
		{name: "global config validation", err: globalconfig.ValidationError{Problems: []string{"global bad"}}, want: cli.ExitValidation},
		{name: "global config github token validation", err: globalconfig.ValidationError{Problems: []string{"github_token: must be a string"}}, want: cli.ExitValidation},
		{name: "global config parse", err: globalconfig.ParseError{Path: "global.yaml", Err: errors.New("bad yaml")}, want: cli.ExitValidation},
		{name: "workflow config validation", err: workflowconfig.ValidationError{Problems: []string{"workflow bad"}}, want: cli.ExitValidation},
		{name: "cli project missing", err: fmt.Errorf("wrapped: %w", cli.ErrProjectNotFound), want: cli.ExitNotFoundOrConfig},
		{name: "cli config exists", err: fmt.Errorf("wrapped: %w", cli.ErrConfigExists), want: cli.ExitNotFoundOrConfig},
		{name: "cli project exists", err: fmt.Errorf("wrapped: %w", cli.ErrProjectExists), want: cli.ExitNotFoundOrConfig},
		{name: "manager project missing", err: fmt.Errorf("wrapped: %w", project.ErrProjectNotFound), want: cli.ExitNotFoundOrConfig},
		{name: "manager project exists", err: fmt.Errorf("wrapped: %w", project.ErrProjectExists), want: cli.ExitNotFoundOrConfig},
		{name: "github project missing", err: fmt.Errorf("wrapped: %w", githubconnector.ErrProjectNotFound), want: cli.ExitNotFoundOrConfig},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := cli.ExitCode(tt.err); got != tt.want {
				t.Fatalf("ExitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}
