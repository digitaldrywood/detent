package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

const (
	errorCodeGeneral         = "general"
	errorCodeValidation      = "validation"
	errorCodeUnknownCommand  = "unknown_command"
	errorCodeUnknownFlag     = "unknown_flag"
	errorCodeGitHubAuth      = "github_auth"
	errorCodeConfigExists    = "config_exists"
	errorCodeProjectExists   = "project_exists"
	errorCodeProjectNotFound = "project_not_found"
	errorCodeDoctorFailed    = "doctor_failed"
	errorCodeShutdown        = "shutdown"
)

type ClassifiedError struct {
	Err        error
	Code       string
	Detail     string
	Hint       string
	DidYouMean []string
}

type Problem struct {
	Type         string   `json:"type"`
	Code         string   `json:"code"`
	Title        string   `json:"title"`
	Detail       string   `json:"detail"`
	ExitCode     int      `json:"exit_code"`
	SuggestedFix string   `json:"suggested_fix,omitempty"`
	DidYouMean   []string `json:"did_you_mean,omitempty"`
	DocsURL      string   `json:"docs_url,omitempty"`
}

func (e *ClassifiedError) Error() string {
	if e == nil {
		return ""
	}
	detail := strings.TrimSpace(e.Detail)
	if detail == "" && e.Err != nil {
		detail = e.Err.Error()
	}
	hint := strings.TrimSpace(e.Hint)
	if hint == "" {
		return detail
	}
	return detail + "\n" + hint
}

func (e *ClassifiedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func NewValidationError(detail string, hint string, didYouMean []string) error {
	return &ClassifiedError{
		Err:        WrapValidation(errors.New(detail)),
		Code:       errorCodeValidation,
		Detail:     detail,
		Hint:       hint,
		DidYouMean: compactStrings(didYouMean),
	}
}

func NewClassifiedError(err error, code string, detail string, hint string, didYouMean []string) error {
	if err == nil {
		err = errors.New(detail)
	}
	return &ClassifiedError{
		Err:        err,
		Code:       code,
		Detail:     detail,
		Hint:       hint,
		DidYouMean: compactStrings(didYouMean),
	}
}

func ProblemForError(err error) Problem {
	code := classifyErrorCode(err)
	detail := ErrorDetail(err)
	didYouMean := ErrorDidYouMean(err)
	problem := Problem{
		Type:         "https://detent.dev/errors/" + code,
		Code:         code,
		Title:        errorTitle(code),
		Detail:       detail,
		ExitCode:     problemExitCode(err, code),
		SuggestedFix: ErrorHint(err),
		DidYouMean:   didYouMean,
		DocsURL:      "https://detent.dev/docs/cli#" + code,
	}
	if problem.SuggestedFix == "" {
		problem.SuggestedFix = defaultSuggestedFix(code, didYouMean)
	}
	return problem
}

func ProblemForCommandError(cmd *cobra.Command, err error) Problem {
	problem := ProblemForError(err)
	if problem.Code != errorCodeUnknownCommand || len(problem.DidYouMean) > 0 {
		return problem
	}
	input := unknownCommandName(problem.Detail)
	if input == "" || cmd == nil {
		return problem
	}
	suggestions := cmd.SuggestionsFor(input)
	if len(suggestions) == 0 {
		return problem
	}
	problem.DidYouMean = suggestions
	problem.SuggestedFix = defaultSuggestedFix(problem.Code, suggestions)
	return problem
}

func problemExitCode(err error, code string) int {
	switch code {
	case errorCodeGitHubAuth:
		return ExitAuth
	case errorCodeValidation, errorCodeUnknownCommand, errorCodeUnknownFlag:
		return ExitValidation
	case errorCodeConfigExists, errorCodeProjectExists, errorCodeProjectNotFound:
		return ExitNotFoundOrConfig
	default:
		return ExitCode(err)
	}
}

func ErrorDetail(err error) string {
	var classified *ClassifiedError
	if errors.As(err, &classified) && strings.TrimSpace(classified.Detail) != "" {
		return strings.TrimSpace(classified.Detail)
	}
	if err == nil {
		return ""
	}
	return firstNonEmptyLine(err.Error())
}

func ErrorHint(err error) string {
	var classified *ClassifiedError
	if errors.As(err, &classified) {
		return strings.TrimSpace(classified.Hint)
	}
	if hint, _, ok := HintFor(err); ok {
		return strings.TrimSpace(hint)
	}
	return ""
}

func ErrorDidYouMean(err error) []string {
	var classified *ClassifiedError
	if errors.As(err, &classified) {
		return compactStrings(classified.DidYouMean)
	}
	if hint, _, ok := HintFor(err); ok {
		return parseDidYouMean(hint)
	}
	if err == nil {
		return nil
	}
	return parseDidYouMean(err.Error())
}

func WriteProblemJSON(out io.Writer, problem Problem) error {
	return WriteJSON(out, problem)
}

func WritePrettyError(out io.Writer, err error) error {
	if out == nil {
		return nil
	}
	if _, writeErr := fmt.Fprintf(out, "Error: %s\n", ErrorDetail(err)); writeErr != nil {
		return writeErr
	}
	if hint := ErrorHint(err); hint != "" {
		_, writeErr := fmt.Fprintf(out, "Hint: %s\n", hint)
		return writeErr
	}
	return nil
}

func classifyErrorCode(err error) string {
	var classified *ClassifiedError
	if errors.As(err, &classified) && classified.Code != "" {
		return classified.Code
	}
	var hinted *HintedError
	if errors.As(err, &hinted) && hinted.Err == nil {
		return errorCodeValidation
	}
	switch {
	case errors.Is(err, ErrGitHubAuth):
		return errorCodeGitHubAuth
	case errors.Is(err, ErrConfigExists):
		return errorCodeConfigExists
	case errors.Is(err, ErrProjectExists):
		return errorCodeProjectExists
	case errors.Is(err, ErrProjectNotFound):
		return errorCodeProjectNotFound
	case errors.Is(err, ErrDoctorFailed):
		return errorCodeDoctorFailed
	case errors.Is(err, ErrShutdownForced), errors.Is(err, ErrShutdownTimeout):
		return errorCodeShutdown
	}
	text := ""
	if err != nil {
		text = err.Error()
	}
	switch {
	case strings.Contains(text, "unknown command"):
		return errorCodeUnknownCommand
	case strings.Contains(text, "unknown flag"):
		return errorCodeUnknownFlag
	case strings.Contains(text, githubAuthHint), strings.Contains(text, "github_token"):
		return errorCodeGitHubAuth
	case errors.Is(err, ErrValidation), errors.Is(err, ErrInvalidOutputFormat):
		return errorCodeValidation
	default:
		return errorCodeGeneral
	}
}

func errorTitle(code string) string {
	switch code {
	case errorCodeValidation:
		return "Validation failed"
	case errorCodeUnknownCommand:
		return "Unknown command"
	case errorCodeUnknownFlag:
		return "Unknown flag"
	case errorCodeGitHubAuth:
		return "GitHub authentication required"
	case errorCodeConfigExists:
		return "Global config already exists"
	case errorCodeProjectExists:
		return "Project already exists"
	case errorCodeProjectNotFound:
		return "Project not found"
	case errorCodeDoctorFailed:
		return "Doctor checks failed"
	case errorCodeShutdown:
		return "Shutdown failed"
	default:
		return "Command failed"
	}
}

func defaultSuggestedFix(code string, didYouMean []string) string {
	switch {
	case code == errorCodeUnknownCommand && len(didYouMean) > 0:
		return "Run detent " + didYouMean[0] + " instead."
	case code == errorCodeUnknownFlag && len(didYouMean) > 0:
		return "Use " + didYouMean[0] + " instead."
	default:
		return ""
	}
}

func firstNonEmptyLine(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return strings.TrimSpace(text)
}

func parseDidYouMean(text string) []string {
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if !strings.Contains(strings.ToLower(line), "did you mean") {
			continue
		}
		var suggestions []string
		for _, candidate := range lines[index+1:] {
			candidate = strings.TrimSpace(candidate)
			if candidate == "" {
				continue
			}
			suggestions = append(suggestions, candidate)
		}
		return compactStrings(suggestions)
	}
	if start := strings.Index(strings.ToLower(text), "did you mean "); start >= 0 {
		candidate := strings.TrimSpace(text[start+len("did you mean "):])
		if after, ok := strings.CutPrefix(candidate, "\""); ok {
			candidate = after
			if end := strings.Index(candidate, "\""); end >= 0 {
				candidate = candidate[:end]
			}
		} else if fields := strings.Fields(candidate); len(fields) > 0 {
			candidate = fields[0]
		}
		candidate = strings.Trim(candidate, "?`\"")
		return compactStrings([]string{candidate})
	}
	return nil
}

func unknownCommandName(detail string) string {
	const prefix = "unknown command "
	detail = strings.TrimSpace(detail)
	if !strings.HasPrefix(detail, prefix) {
		return ""
	}
	remainder := strings.TrimSpace(strings.TrimPrefix(detail, prefix))
	if !strings.HasPrefix(remainder, "\"") {
		return ""
	}
	remainder = strings.TrimPrefix(remainder, "\"")
	before, _, ok := strings.Cut(remainder, "\"")
	if !ok {
		return ""
	}
	return before
}

func compactStrings(values []string) []string {
	var compacted []string
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		compacted = append(compacted, value)
	}
	return compacted
}
