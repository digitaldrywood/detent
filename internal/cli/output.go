package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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

type CommandCatalog struct {
	Commands []CommandCatalogEntry `json:"commands"`
}

type CommandCatalogEntry struct {
	Name    string               `json:"name"`
	Use     string               `json:"use"`
	Short   string               `json:"short"`
	Args    string               `json:"args,omitempty"`
	Example string               `json:"example"`
	Flags   []CommandCatalogFlag `json:"flags,omitempty"`
}

type CommandCatalogFlag struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Usage     string `json:"usage"`
	Default   string `json:"default,omitempty"`
}

func AddFormatFlag(cmd *cobra.Command, value *string) {
	cmd.PersistentFlags().StringVar(value, outputFormatFlag, "", "output format: pretty or json (default: pretty on TTY, json when piped; DETENT_FORMAT overrides)")
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

func ResolveCommandOutputFormat(cmd *cobra.Command, lookupEnv func(string) string, stdoutTTY bool) (OutputFormat, error) {
	value, set := outputFormatFlagValue(cmd)
	envValue := ""
	if lookupEnv == nil {
		lookupEnv = os.Getenv
	}
	envValue = lookupEnv(outputFormatEnv)
	return ResolveOutputFormat(value, set, envValue, stdoutTTY)
}

func ResolveOutputFormatFromArgs(args []string, lookupEnv func(string) string, stdoutTTY bool) (OutputFormat, error) {
	value, set := outputFormatArg(args)
	if lookupEnv == nil {
		lookupEnv = os.Getenv
	}
	return ResolveOutputFormat(value, set, lookupEnv(outputFormatEnv), stdoutTTY)
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

func WriteCommandOutput(out io.Writer, format OutputFormat, value any, pretty func(io.Writer) error) error {
	return CommandOutput{Format: format, Out: out}.Write(pretty, value)
}

func WriteJSON(out io.Writer, value any) error {
	if out == nil {
		out = io.Discard
	}
	return writeJSON(out, value)
}

func WriterIsTTY(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func NewCommandCatalog(root *cobra.Command) CommandCatalog {
	var catalog CommandCatalog
	walkCommandCatalog(root, root.CommandPath(), &catalog)
	return catalog
}

func commandOutputFormat(cmd *cobra.Command, opts options) (OutputFormat, error) {
	stdoutTTY := false
	if opts.stdoutTTY != nil {
		stdoutTTY = opts.stdoutTTY()
	}
	return ResolveCommandOutputFormat(cmd, opts.lookupEnv, stdoutTTY)
}

func parseOutputFormat(value string, source string) (OutputFormat, error) {
	switch OutputFormat(strings.ToLower(strings.TrimSpace(value))) {
	case OutputFormatPretty:
		return OutputFormatPretty, nil
	case OutputFormatJSON:
		return OutputFormatJSON, nil
	default:
		detail := fmt.Sprintf("%s must be pretty or json", source)
		return "", NewClassifiedError(
			WrapValidation(fmt.Errorf("%w: %s", ErrInvalidOutputFormat, detail)),
			errorCodeValidation,
			detail,
			"Use --format pretty for human-readable output or --format json for machine-readable output.",
			nil,
		)
	}
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

func outputFormatArg(args []string) (string, bool) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			return "", false
		}
		if arg == "--format" {
			if index+1 >= len(args) {
				return "", true
			}
			return args[index+1], true
		}
		if strings.HasPrefix(arg, "--format=") {
			return strings.TrimPrefix(arg, "--format="), true
		}
	}
	return "", false
}

func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func walkCommandCatalog(cmd *cobra.Command, rootPath string, catalog *CommandCatalog) {
	if cmd == nil || cmd.Hidden {
		return
	}
	catalog.Commands = append(catalog.Commands, catalogEntry(cmd, rootPath))
	for _, child := range cmd.Commands() {
		if child.Name() == "completion" {
			continue
		}
		walkCommandCatalog(child, rootPath, catalog)
	}
}

func catalogEntry(cmd *cobra.Command, rootPath string) CommandCatalogEntry {
	name := cmd.CommandPath()
	if rootPath != "" && name != rootPath {
		name = strings.TrimPrefix(name, rootPath+" ")
	}
	return CommandCatalogEntry{
		Name:    name,
		Use:     cmd.UseLine(),
		Short:   strings.TrimSpace(cmd.Short),
		Args:    commandArgsDisplay(cmd),
		Example: strings.TrimSpace(cmd.Example),
		Flags:   catalogFlags(cmd),
	}
}

func commandArgsDisplay(cmd *cobra.Command) string {
	use := strings.TrimSpace(cmd.Use)
	name := strings.TrimSpace(cmd.Name())
	if use == "" || name == "" || use == name {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(use, name))
}

func catalogFlags(cmd *cobra.Command) []CommandCatalogFlag {
	seen := map[string]CommandCatalogFlag{}
	addFlags := func(flags *pflag.FlagSet) {
		if flags == nil {
			return
		}
		flags.VisitAll(func(flag *pflag.Flag) {
			if flag.Hidden {
				return
			}
			seen[flag.Name] = CommandCatalogFlag{
				Name:      flag.Name,
				Shorthand: flag.Shorthand,
				Usage:     flag.Usage,
				Default:   flag.DefValue,
			}
		})
	}

	addFlags(cmd.InheritedFlags())
	addFlags(cmd.PersistentFlags())
	addFlags(cmd.LocalFlags())

	flags := make([]CommandCatalogFlag, 0, len(seen))
	for _, flag := range seen {
		flags = append(flags, flag)
	}
	sort.Slice(flags, func(i, j int) bool {
		return flags[i].Name < flags[j].Name
	})
	return flags
}
