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
	errorCodeShutdownForced  = "shutdown_forced"
	errorCodeShutdownTimeout = "shutdown_timeout"
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
	class := classifyError(err)
	code := class.Slug
	detail := ErrorDetail(err)
	didYouMean := ErrorDidYouMean(err)
	problem := Problem{
		Type:         "https://detent.dev/errors/" + code,
		Code:         code,
		Title:        class.Title,
		Detail:       detail,
		ExitCode:     class.ExitCode,
		SuggestedFix: ErrorHint(err),
		DidYouMean:   didYouMean,
		DocsURL:      docsURLForErrorCode(code),
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

func docsURLForErrorCode(code string) string {
	return "https://detent.dev/docs/cli#" + strings.ReplaceAll(code, "_", "-")
}

func parseDidYouMean(text string) []string {
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if !strings.Contains(strings.ToLower(line), "did you mean") {
			continue
		}
		if suggestion := inlineDidYouMeanSuggestion(line); suggestion != "" {
			return []string{suggestion}
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
	return nil
}

func inlineDidYouMeanSuggestion(line string) string {
	lower := strings.ToLower(line)
	start := strings.Index(lower, "did you mean")
	if start < 0 {
		return ""
	}
	candidate := strings.TrimSpace(line[start+len("did you mean"):])
	candidate = strings.TrimLeft(candidate, " :")
	if candidate == "" || strings.HasPrefix(strings.ToLower(candidate), "this") {
		return ""
	}
	if after, ok := strings.CutPrefix(candidate, "\""); ok {
		if end := strings.Index(after, "\""); end >= 0 {
			return strings.TrimSpace(after[:end])
		}
		candidate = after
	} else if fields := strings.Fields(candidate); len(fields) > 0 {
		candidate = fields[0]
	}
	return strings.Trim(candidate, "?`\"")
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
