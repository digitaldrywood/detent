package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

type OutputFormat string

const (
	OutputFormatPretty OutputFormat = "pretty"
	OutputFormatJSON   OutputFormat = "json"

	outputFormatFlag = "format"
	outputFormatEnv  = "DETENT_FORMAT"
)

var ErrInvalidOutputFormat = errors.New("invalid output format")

type commandOutputContextKey struct{}

type commandOutputOptions struct {
	lookupEnv func(string) string
	stdoutTTY func() bool
}

type CommandOutput struct {
	Format OutputFormat
	Out    io.Writer
}

func withCommandOutputOptions(ctx context.Context, opts commandOutputOptions) context.Context {
	return context.WithValue(ctx, commandOutputContextKey{}, opts)
}

func OutputForCommand(cmd *cobra.Command) (CommandOutput, error) {
	opts := commandOutputOptions{
		lookupEnv: os.Getenv,
		stdoutTTY: func() bool {
			if cmd == nil {
				return WriterIsTTY(os.Stdout)
			}
			return WriterIsTTY(cmd.OutOrStdout())
		},
	}
	if cmd != nil {
		if root := cmd.Root(); root != nil && root.Context() != nil {
			if configured, ok := root.Context().Value(commandOutputContextKey{}).(commandOutputOptions); ok {
				if configured.lookupEnv != nil {
					opts.lookupEnv = configured.lookupEnv
				}
				if configured.stdoutTTY != nil {
					opts.stdoutTTY = configured.stdoutTTY
				}
			}
		}
	}

	formatFlag, flagSet := outputFormatFlagValue(cmd)
	format, err := ResolveOutputFormat(formatFlag, flagSet, opts.lookupEnv(outputFormatEnv), opts.stdoutTTY())
	if err != nil {
		return CommandOutput{}, err
	}
	out := io.Discard
	if cmd != nil {
		out = cmd.OutOrStdout()
	}
	return CommandOutput{Format: format, Out: out}, nil
}

func ResolveOutputFormat(flagValue string, flagSet bool, envValue string, stdoutTTY bool) (OutputFormat, error) {
	if flagSet {
		return parseOutputFormat(flagValue, "--"+outputFormatFlag)
	}
	if strings.TrimSpace(envValue) != "" {
		return parseOutputFormat(envValue, outputFormatEnv)
	}
	if stdoutTTY {
		return OutputFormatPretty, nil
	}
	return OutputFormatJSON, nil
}

func parseOutputFormat(value string, source string) (OutputFormat, error) {
	switch OutputFormat(strings.ToLower(strings.TrimSpace(value))) {
	case OutputFormatPretty:
		return OutputFormatPretty, nil
	case OutputFormatJSON:
		return OutputFormatJSON, nil
	default:
		return "", fmt.Errorf("%w for %s: %q (want pretty or json)", ErrInvalidOutputFormat, source, value)
	}
}

func (o CommandOutput) IsJSON() bool {
	return o.Format == OutputFormatJSON
}

func (o CommandOutput) Write(pretty func(io.Writer) error, value any) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.IsJSON() {
		return writeJSON(o.Out, value)
	}
	if pretty == nil {
		return nil
	}
	return pretty(o.Out)
}

func (o CommandOutput) WriteJSON(value any) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	return writeJSON(o.Out, value)
}

func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func outputFormatFlagValue(cmd *cobra.Command) (string, bool) {
	if cmd == nil {
		return "", false
	}
	flag := cmd.Flags().Lookup(outputFormatFlag)
	if flag == nil {
		flag = cmd.InheritedFlags().Lookup(outputFormatFlag)
	}
	if flag == nil {
		flag = cmd.PersistentFlags().Lookup(outputFormatFlag)
	}
	if flag == nil && cmd.Root() != nil {
		flag = cmd.Root().PersistentFlags().Lookup(outputFormatFlag)
	}
	if flag == nil {
		return "", false
	}
	return flag.Value.String(), flag.Changed
}

func WriterIsTTY(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
