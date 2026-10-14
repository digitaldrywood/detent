package cli

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	flagSuggestionMaxDistance = 2
	didYouMeanHeading         = "Did you mean this?"
)

func ConfigureCommandSuggestions(root *cobra.Command) {
	if root == nil {
		return
	}
	root.SuggestionsMinimumDistance = flagSuggestionMaxDistance
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return WrapValidation(flagSuggestionError(cmd, err))
	})
	setSuggestFor(root, "remove-project", "rm", "delete", "remove")
	setSuggestFor(root, "add-project", "add", "new")
	setSuggestFor(root, "unpause", "resume", "start")
	setSuggestFor(root, "pause", "stop")
	setSuggestFor(root, "promote", "prioritize")
}

func setSuggestFor(root *cobra.Command, name string, suggestions ...string) {
	cmd := findCommandByName(root, name)
	if cmd != nil {
		seen := map[string]bool{}
		for _, suggestion := range cmd.SuggestFor {
			seen[suggestion] = true
		}
		for _, suggestion := range suggestions {
			if !seen[suggestion] {
				cmd.SuggestFor = append(cmd.SuggestFor, suggestion)
				seen[suggestion] = true
			}
		}
	}
}

func findCommandByName(cmd *cobra.Command, name string) *cobra.Command {
	if cmd == nil {
		return nil
	}
	if cmd.Name() == name {
		return cmd
	}
	for _, child := range cmd.Commands() {
		if found := findCommandByName(child, name); found != nil {
			return found
		}
	}
	return nil
}

func suggestedNoArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return WrapValidation(unknownCommandError(cmd, args[0]))
}

func unknownCommandError(cmd *cobra.Command, typedName string) error {
	return fmt.Errorf("unknown command %q for %q%s", typedName, cmd.CommandPath(), commandSuggestionText(cmd, typedName))
}

func commandSuggestionText(cmd *cobra.Command, typedName string) string {
	if cmd.DisableSuggestions {
		return ""
	}
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = flagSuggestionMaxDistance
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
