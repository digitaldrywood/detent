package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

type errorClass struct {
	ErrorClass
	Title string
	match func(error) bool
}

var (
	successErrorClass = errorClass{ErrorClass: ErrorClass{Slug: "success", ExitCode: ExitSuccess}, Title: "Success"}
	generalErrorClass = errorClass{ErrorClass: ErrorClass{Slug: errorCodeGeneral, ExitCode: ExitGeneral}, Title: "Command failed"}
)

var errorClassifiers = []errorClass{
	{
		ErrorClass: ErrorClass{Slug: errorCodeUnknownCommand, ExitCode: ExitValidation},
		Title:      "Unknown command",
		match: func(err error) bool {
			return errorTextContains(err, "unknown command")
		},
	},
	{
		ErrorClass: ErrorClass{Slug: errorCodeUnknownFlag, ExitCode: ExitValidation},
		Title:      "Unknown flag",
		match: func(err error) bool {
			return errorTextContains(err, "unknown flag")
		},
	},
	{
		ErrorClass: ErrorClass{Slug: errorCodeGitHubAuth, ExitCode: ExitAuth},
		Title:      "GitHub authentication required",
		match: func(err error) bool {
			return errors.Is(err, ErrGitHubAuth) ||
				errors.Is(err, githubconnector.ErrMissingToken) ||
				errors.Is(err, githubconnector.ErrAuthenticationFailed) ||
				errorTextContains(err, githubAuthHint) ||
				errorTextContains(err, "github_token")
		},
	},
	{
		ErrorClass: ErrorClass{Slug: errorCodeConfigExists, ExitCode: ExitNotFoundOrConfig},
		Title:      "Global config already exists",
		match: func(err error) bool {
			return errors.Is(err, ErrConfigExists)
		},
	},
	{
		ErrorClass: ErrorClass{Slug: errorCodeProjectExists, ExitCode: ExitNotFoundOrConfig},
		Title:      "Project already exists",
		match: func(err error) bool {
			return errors.Is(err, ErrProjectExists) ||
				errors.Is(err, project.ErrProjectExists)
		},
	},
	{
		ErrorClass: ErrorClass{Slug: errorCodeProjectNotFound, ExitCode: ExitNotFoundOrConfig},
		Title:      "Project not found",
		match: func(err error) bool {
			return errors.Is(err, ErrProjectNotFound) ||
				errors.Is(err, project.ErrProjectNotFound) ||
				errors.Is(err, githubconnector.ErrProjectNotFound) ||
				errors.Is(err, githubconnector.ErrProjectItemNotFound) ||
				errors.Is(err, githubconnector.ErrProjectFieldNotFound) ||
				errors.Is(err, githubconnector.ErrProjectFieldOptionNotFound) ||
				errors.Is(err, githubconnector.ErrStatusFieldNotFound) ||
				errors.Is(err, githubconnector.ErrStatusOptionNotFound)
		},
	},
	{
		ErrorClass: ErrorClass{Slug: errorCodeDoctorFailed, ExitCode: ExitGeneral},
		Title:      "Doctor checks failed",
		match: func(err error) bool {
			return errors.Is(err, ErrDoctorFailed)
		},
	},
	{
		ErrorClass: ErrorClass{Slug: errorCodeShutdownForced, ExitCode: ExitGeneral},
		Title:      "Shutdown forced",
		match: func(err error) bool {
			return errors.Is(err, ErrShutdownForced)
		},
	},
	{
		ErrorClass: ErrorClass{Slug: errorCodeShutdownTimeout, ExitCode: ExitGeneral},
		Title:      "Shutdown timed out",
		match: func(err error) bool {
			return errors.Is(err, ErrShutdownTimeout)
		},
	},
	{
		ErrorClass: ErrorClass{Slug: errorCodeValidation, ExitCode: ExitValidation},
		Title:      "Validation failed",
		match: func(err error) bool {
			var globalValidation globalconfig.ValidationError
			var globalParse globalconfig.ParseError
			var workflowValidation workflowconfig.ValidationError
			var hinted *HintedError
			return errors.Is(err, ErrValidation) ||
				errors.Is(err, ErrInvalidOutputFormat) ||
				(errors.As(err, &hinted) && hinted.Err == nil) ||
				errors.As(err, &globalValidation) ||
				errors.As(err, &globalParse) ||
				errors.As(err, &workflowValidation)
		},
	},
}

func ClassifyError(err error) ErrorClass {
	return classifyError(err).ErrorClass
}

func classifyError(err error) errorClass {
	if err == nil || errors.Is(err, context.Canceled) {
		return successErrorClass
	}

	var classified *ClassifiedError
	if errors.As(err, &classified) && classified.Code != "" {
		if class, ok := errorClassForSlug(classified.Code); ok {
			return class
		}
		exitCode := ExitGeneral
		if classified.Err != nil {
			exitCode = ClassifyError(classified.Err).ExitCode
		}
		return errorClass{
			ErrorClass: ErrorClass{Slug: classified.Code, ExitCode: exitCode},
			Title:      generalErrorClass.Title,
		}
	}

	for _, classifier := range errorClassifiers {
		if classifier.match(err) {
			return classifier
		}
	}
	return generalErrorClass
}

func ExitCode(err error) int {
	return ClassifyError(err).ExitCode
}

func errorClassForSlug(slug string) (errorClass, bool) {
	if slug == successErrorClass.Slug {
		return successErrorClass, true
	}
	if slug == generalErrorClass.Slug {
		return generalErrorClass, true
	}
	for _, class := range errorClassifiers {
		if class.Slug == slug {
			return class, true
		}
	}
	return errorClass{}, false
}

func errorTextContains(err error, text string) bool {
	return err != nil && text != "" && strings.Contains(err.Error(), text)
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
