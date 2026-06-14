package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/project"
)

const (
	ExitSuccess          = 0
	ExitGeneral          = 1
	ExitAuth             = 2
	ExitValidation       = 3
	ExitNotFoundOrConfig = 4
)

var (
	ErrGitHubAuth = errors.New("github auth failed")
	ErrValidation = errors.New("input validation failed")
)

type ErrorClass struct {
	Slug     string
	ExitCode int
}

var errorClassifiers = []struct {
	class ErrorClass
	match func(error) bool
}{
	{
		class: ErrorClass{Slug: "github_auth", ExitCode: ExitAuth},
		match: func(err error) bool {
			return errors.Is(err, ErrGitHubAuth) ||
				errors.Is(err, githubconnector.ErrMissingToken) ||
				errors.Is(err, githubconnector.ErrAuthenticationFailed)
		},
	},
	{
		class: ErrorClass{Slug: "validation", ExitCode: ExitValidation},
		match: func(err error) bool {
			var globalValidation globalconfig.ValidationError
			var globalParse globalconfig.ParseError
			var workflowValidation workflowconfig.ValidationError
			return errors.Is(err, ErrValidation) ||
				errors.Is(err, ErrInvalidOutputFormat) ||
				errors.As(err, &globalValidation) ||
				errors.As(err, &globalParse) ||
				errors.As(err, &workflowValidation)
		},
	},
	{
		class: ErrorClass{Slug: "not_found_or_config", ExitCode: ExitNotFoundOrConfig},
		match: func(err error) bool {
			return errors.Is(err, ErrConfigExists) ||
				errors.Is(err, ErrProjectExists) ||
				errors.Is(err, ErrProjectNotFound) ||
				errors.Is(err, project.ErrProjectExists) ||
				errors.Is(err, project.ErrProjectNotFound) ||
				errors.Is(err, githubconnector.ErrProjectNotFound) ||
				errors.Is(err, githubconnector.ErrProjectItemNotFound) ||
				errors.Is(err, githubconnector.ErrProjectFieldNotFound) ||
				errors.Is(err, githubconnector.ErrProjectFieldOptionNotFound) ||
				errors.Is(err, githubconnector.ErrStatusFieldNotFound) ||
				errors.Is(err, githubconnector.ErrStatusOptionNotFound)
		},
	},
}

func ClassifyError(err error) ErrorClass {
	if err == nil || errors.Is(err, context.Canceled) {
		return ErrorClass{Slug: "success", ExitCode: ExitSuccess}
	}
	for _, classifier := range errorClassifiers {
		if classifier.match(err) {
			return classifier.class
		}
	}
	return ErrorClass{Slug: "general", ExitCode: ExitGeneral}
}

func ExitCode(err error) int {
	return ClassifyError(err).ExitCode
}

func NoArgs(cmd *cobra.Command, args []string) error {
	return WrapValidation(cobra.NoArgs(cmd, args))
}

func ExactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		return WrapValidation(cobra.ExactArgs(n)(cmd, args))
	}
}

func WrapValidation(err error) error {
	if err == nil || errors.Is(err, ErrValidation) {
		return err
	}
	return classifiedError{class: ErrValidation, err: err}
}

func ValidationError(message string) error {
	return WrapValidation(errors.New(message))
}

func ValidationErrorf(format string, args ...any) error {
	return WrapValidation(fmt.Errorf(format, args...))
}

func GitHubAuthError(err error) error {
	if err == nil || errors.Is(err, ErrGitHubAuth) {
		return err
	}
	return classifiedError{class: ErrGitHubAuth, err: err}
}

type classifiedError struct {
	class error
	err   error
}

func (e classifiedError) Error() string {
	if e.err == nil {
		return e.class.Error()
	}
	return e.err.Error()
}

func (e classifiedError) Unwrap() []error {
	switch {
	case e.class == nil && e.err == nil:
		return nil
	case e.class == nil:
		return []error{e.err}
	case e.err == nil:
		return []error{e.class}
	default:
		return []error{e.class, e.err}
	}
}
