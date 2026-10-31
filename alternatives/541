package cli

import (
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/logging"
)

func serveLogger(cmd *cobra.Command, lookupEnv func(string) string, output io.Writer) (*slog.Logger, string, error) {
	var flag runtimeStringFlag
	if value := cmd.Flag("log-level"); value != nil {
		flag = runtimeStringFlag{Value: value.Value.String(), Set: flagChanged(cmd, "log-level")}
	}
	level := resolveRuntimeString(runtimeStringInput{
		Flag:          flag,
		EnvCandidates: []string{"LOG_LEVEL", "DETENT_LOG_LEVEL"},
		DefaultValue:  defaultRuntimeLogLevel,
	}, lookupEnv)
	if !validSlogLevel(level.Value) {
		return nil, "", NewValidationError("log level "+level.Value+" from "+level.Source+" is invalid", "Use debug, info, warn, or error.", nil)
	}
	return slog.New(logging.NewHandler(output, parseSlogLevel(level.Value), false, logging.SourceFromEnv(lookupEnv))), strings.ToLower(strings.TrimSpace(level.Value)), nil
}

func validSlogLevel(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug", "info", "warn", "warning", "error":
		return true
	default:
		return false
	}
}
