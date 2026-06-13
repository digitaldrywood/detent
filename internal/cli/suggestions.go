package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	outputFormatFlagName      = "format"
	outputFormatPretty        = "pretty"
	outputFormatJSON          = "json"
	flagSuggestionMaxDistance = 2
	didYouMeanHeading         = "Did you mean this?"
	unknownCommandErrorPrefix = `unknown command "`
	unknownFlagErrorPrefix    = "unknown flag: "
	commandFailedErrorCode    = "command_failed"
	unknownCommandErrorCode   = "unknown_command"
	unknownFlagErrorCode      = "unknown_flag"
)

type outputFormatValue struct {
	value *string
}

func newOutputFormatValue(value *string) outputFormatValue {
	if value != nil && strings.TrimSpace(*value) == "" {
		*value = outputFormatPretty
	}
	return outputFormatValue{value: value}
}

func (v outputFormatValue) String() string {
	if v.value == nil || strings.TrimSpace(*v.value) == "" {
		return outputFormatPretty
	}
	return *v.value
}

func (v outputFormatValue) Set(value string) error {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case outputFormatPretty, outputFormatJSON:
		if v.value != nil {
			*v.value = value
		}
		return nil
	default:
		return fmt.Errorf("output format must be %q or %q", outputFormatPretty, outputFormatJSON)
	}
}

func (v outputFormatValue) Type() string {
	return "format"
}

func CommandOutputIsJSON(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	root := cmd.Root()
	if root == nil {
		root = cmd
	}
	flag := root.PersistentFlags().Lookup(outputFormatFlagName)
	if flag == nil {
		flag = root.Flags().Lookup(outputFormatFlagName)
	}
	return flag != nil && strings.EqualFold(flag.Value.String(), outputFormatJSON)
}

type commandErrorResponse struct {
	Error commandError `json:"error"`
}

type commandError struct {
	Code       string   `json:"code"`
	Input      string   `json:"input,omitempty"`
	DidYouMean []string `json:"did_you_mean,omitempty"`
	Message    string   `json:"message,omitempty"`
}

func WriteCommandErrorJSON(out io.Writer, err error) error {
	if out == nil {
		out = io.Discard
	}
	encoder := json.NewEncoder(out)
	return encoder.Encode(commandErrorResponseFromError(err))
}

func commandErrorResponseFromError(err error) commandErrorResponse {
	message := ""
	if err != nil {
		message = err.Error()
	}

	if input, ok := unknownCommandInput(message); ok {
		return commandErrorResponse{Error: commandError{
			Code:       unknownCommandErrorCode,
			Input:      input,
			DidYouMean: didYouMeanSuggestions(message),
		}}
	}
	if input, ok := unknownFlagInput(message); ok {
		return commandErrorResponse{Error: commandError{
			Code:       unknownFlagErrorCode,
			Input:      input,
			DidYouMean: didYouMeanSuggestions(message),
		}}
	}
	return commandErrorResponse{Error: commandError{
		Code:    commandFailedErrorCode,
		Message: firstErrorLine(message),
	}}
}

func suggestedNoArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return unknownCommandError(cmd, args[0])
}

func unknownCommandError(cmd *cobra.Command, typedName string) error {
	return fmt.Errorf("unknown command %q for %q%s", typedName, cmd.CommandPath(), commandSuggestionText(cmd, typedName))
}

func commandSuggestionText(cmd *cobra.Command, typedName string) string {
	if cmd.DisableSuggestions {
		return ""
	}
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	var builder strings.Builder
	if suggestions := cmd.SuggestionsFor(typedName); len(suggestions) > 0 {
		builder.WriteString("\n\n")
		builder.WriteString(didYouMeanHeading)
		builder.WriteString("\n")
		seen := map[string]struct{}{}
		for _, suggestion := range suggestions {
			if _, ok := seen[suggestion]; ok {
				continue
			}
			seen[suggestion] = struct{}{}
			fmt.Fprintf(&builder, "\t%s\n", suggestion)
		}
	}
	return builder.String()
}

func flagSuggestionError(cmd *cobra.Command, err error) error {
	if commandErr := commandSuggestionFromFlagContext(cmd); commandErr != nil {
		return commandErr
	}

	var notExist *pflag.NotExistError
	if !errors.As(err, &notExist) || strings.TrimSpace(notExist.GetSpecifiedShortnames()) != "" {
		return err
	}
	name := strings.TrimSpace(notExist.GetSpecifiedName())
	if name == "" {
		return err
	}
	suggestion := closestFlagName(cmd, name)
	if suggestion == "" {
		return err
	}
	return fmt.Errorf("%w\n\n%s\n\t--%s", err, didYouMeanHeading, suggestion)
}

func commandSuggestionFromFlagContext(cmd *cobra.Command) error {
	if cmd == nil || cmd.HasParent() {
		return nil
	}
	args := cmd.Flags().Args()
	if len(args) == 0 {
		return nil
	}
	typedName := strings.TrimSpace(args[0])
	if typedName == "" || strings.HasPrefix(typedName, "-") {
		return nil
	}
	return unknownCommandError(cmd, typedName)
}

func closestFlagName(cmd *cobra.Command, input string) string {
	names := knownFlagNames(cmd)
	bestName := ""
	bestDistance := flagSuggestionMaxDistance + 1
	for _, name := range names {
		distance := levenshteinDistance(input, name)
		if distance < bestDistance {
			bestName = name
			bestDistance = distance
		}
	}
	if bestDistance > flagSuggestionMaxDistance {
		return ""
	}
	return bestName
}

func knownFlagNames(cmd *cobra.Command) []string {
	if cmd == nil {
		return nil
	}
	seen := map[string]struct{}{}
	addFlags := func(flags *pflag.FlagSet) {
		if flags == nil {
			return
		}
		flags.VisitAll(func(flag *pflag.Flag) {
			if flag == nil || flag.Hidden || flag.Deprecated != "" {
				return
			}
			name := strings.TrimSpace(flag.Name)
			if name == "" {
				return
			}
			seen[name] = struct{}{}
		})
	}
	addFlags(cmd.LocalFlags())
	addFlags(cmd.InheritedFlags())
	addFlags(cmd.Flags())
	addFlags(cmd.PersistentFlags())
	cmd.VisitParents(func(parent *cobra.Command) {
		addFlags(parent.PersistentFlags())
	})

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func unknownCommandInput(message string) (string, bool) {
	if !strings.HasPrefix(message, unknownCommandErrorPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(message, unknownCommandErrorPrefix)
	end := strings.Index(rest, `"`)
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

func unknownFlagInput(message string) (string, bool) {
	line := firstErrorLine(message)
	if !strings.HasPrefix(line, unknownFlagErrorPrefix) {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, unknownFlagErrorPrefix))
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", false
	}
	return fields[0], true
}

func didYouMeanSuggestions(message string) []string {
	index := strings.Index(message, didYouMeanHeading)
	if index < 0 {
		return nil
	}
	rest := message[index+len(didYouMeanHeading):]
	var suggestions []string
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			suggestions = append(suggestions, line)
		}
	}
	return suggestions
}

func firstErrorLine(message string) string {
	line, _, _ := strings.Cut(message, "\n")
	return strings.TrimSpace(line)
}
