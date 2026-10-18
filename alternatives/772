package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

const (
	onboardingGateDiagnosticStatusPass        = "pass"
	onboardingGateDiagnosticStatusFail        = "fail"
	onboardingGateDiagnosticStatusEnvPolluted = "env_polluted"
	onboardingGateDiagnosticTimeout           = 2 * time.Minute
)

type onboardingGateDiagnosticConfig struct {
	SourceRoot string
	Command    string
	Timeout    time.Duration
}

type onboardingGateDiagnosticResult struct {
	Status                    string   `json:"status"`
	SourceRoot                string   `json:"source_root"`
	PassingCommand            string   `json:"passing_command,omitempty"`
	FailingCommand            string   `json:"failing_command,omitempty"`
	RelevantEnvironmentKeys   []string `json:"relevant_environment_keys,omitempty"`
	CandidateEnvironmentFiles []string `json:"candidate_environment_files,omitempty"`
	PassingSanitizedCommand   string   `json:"passing_sanitized_command,omitempty"`
	RecommendedGateCommand    string   `json:"recommended_gate_command"`
	Detail                    string   `json:"detail,omitempty"`
}

type onboardingGateCommandResult struct {
	Passed bool
	Detail string
}

func newOnboardingDiagnoseGateCommand() *cobra.Command {
	var sourceRoot string
	var gateCommand string
	timeout := onboardingGateDiagnosticTimeout

	cmd := &cobra.Command{
		Use:          "diagnose-gate",
		Short:        "Diagnose onboarding validation gate commands",
		Example:      `detent onboarding diagnose-gate --source-root /path/to/repo --command "make test"`,
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			result, err := diagnoseOnboardingGate(cmd.Context(), onboardingGateDiagnosticConfig{
				SourceRoot: sourceRoot,
				Command:    gateCommand,
				Timeout:    timeout,
			})
			if err != nil {
				return err
			}
			return out.Write(func(w io.Writer) error {
				return writeOnboardingGateDiagnosticPretty(w, result)
			}, result)
		},
	}
	cmd.Flags().StringVar(&sourceRoot, "source-root", ".", "target repository checkout root")
	cmd.Flags().StringVar(&gateCommand, "command", "", "validation gate command to run locally")
	cmd.Flags().DurationVar(&timeout, "timeout", onboardingGateDiagnosticTimeout, "per-command timeout")
	return cmd
}

func diagnoseOnboardingGate(ctx context.Context, cfg onboardingGateDiagnosticConfig) (onboardingGateDiagnosticResult, error) {
	command := strings.TrimSpace(cfg.Command)
	if command == "" {
		return onboardingGateDiagnosticResult{}, NewValidationError(
			"--command is required",
			`Run detent onboarding diagnose-gate --source-root /path/to/repo --command "make test".`,
			nil,
		)
	}
	if cfg.Timeout <= 0 {
		return onboardingGateDiagnosticResult{}, NewValidationError(
			"--timeout must be greater than zero",
			"Use a positive duration such as --timeout 2m.",
			nil,
		)
	}

	sourceRoot, err := resolveOnboardingGateSourceRoot(cfg.SourceRoot)
	if err != nil {
		return onboardingGateDiagnosticResult{}, err
	}
	result := onboardingGateDiagnosticResult{
		SourceRoot:             sourceRoot,
		RecommendedGateCommand: command,
	}

	run := runOnboardingGateCommand(ctx, sourceRoot, command, cfg.Timeout, nil)
	if run.Passed {
		result.Status = onboardingGateDiagnosticStatusPass
		result.PassingCommand = command
		result.Detail = "gate command passed with the current environment"
		return result, nil
	}

	result.Status = onboardingGateDiagnosticStatusFail
	result.FailingCommand = command
	result.Detail = run.Detail

	keys, files, err := onboardingCandidateEnvironmentKeys(sourceRoot)
	if err != nil {
		return onboardingGateDiagnosticResult{}, err
	}
	result.CandidateEnvironmentFiles = files
	if len(keys) == 0 {
		return result, nil
	}

	sanitizedRun := runOnboardingGateCommand(ctx, sourceRoot, command, cfg.Timeout, environmentWithoutKeys(keys))
	if !sanitizedRun.Passed {
		return result, nil
	}

	sanitizedCommand := onboardingSanitizedGateCommand(command, keys)
	result.Status = onboardingGateDiagnosticStatusEnvPolluted
	result.RelevantEnvironmentKeys = keys
	result.PassingSanitizedCommand = sanitizedCommand
	result.RecommendedGateCommand = sanitizedCommand
	result.Detail = "gate command failed with the current environment but passed after unsetting candidate environment keys"
	return result, nil
}

func resolveOnboardingGateSourceRoot(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "."
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve source root %s: %w", path, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		if os.IsNotExist(err) {
			return "", NewValidationError(
				"source root not found: "+absolute,
				"Pass --source-root with an existing repository checkout.",
				nil,
			)
		}
		return "", fmt.Errorf("inspect source root %s: %w", absolute, err)
	}
	if !info.IsDir() {
		return "", NewValidationError(
			"source root is not a directory: "+absolute,
			"Pass --source-root with an existing repository checkout.",
			nil,
		)
	}
	return filepath.Clean(absolute), nil
}

func runOnboardingGateCommand(ctx context.Context, sourceRoot string, command string, timeout time.Duration, env []string) onboardingGateCommandResult {
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := commandshell.Command(commandCtx, command, "")
	cmd.Dir = sourceRoot
	if env != nil {
		cmd.Env = env
	}
	output, err := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		err = commandCtx.Err()
	}
	detail := strings.TrimSpace(string(output))
	if err != nil {
		if detail != "" {
			return onboardingGateCommandResult{Detail: err.Error() + ": " + detail}
		}
		return onboardingGateCommandResult{Detail: err.Error()}
	}
	return onboardingGateCommandResult{Passed: true, Detail: detail}
}

func onboardingCandidateEnvironmentKeys(sourceRoot string) ([]string, []string, error) {
	files, err := onboardingCandidateEnvironmentFiles(sourceRoot)
	if err != nil {
		return nil, nil, err
	}
	seenFiles := make([]string, 0, len(files))
	seenKeys := map[string]bool{}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, fmt.Errorf("read environment candidate file %s: %w", path, err)
		}
		seenFiles = append(seenFiles, path)
		for _, key := range parseOnboardingEnvironmentKeys(raw) {
			if _, ok := os.LookupEnv(key); ok {
				seenKeys[key] = true
			}
		}
	}
	keys := make([]string, 0, len(seenKeys))
	for key := range seenKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, seenFiles, nil
}

func onboardingCandidateEnvironmentFiles(sourceRoot string) ([]string, error) {
	entries, err := os.ReadDir(sourceRoot)
	if err != nil {
		return nil, fmt.Errorf("read source root %s: %w", sourceRoot, err)
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".env") {
			continue
		}
		files = append(files, filepath.Join(sourceRoot, entry.Name()))
	}
	sort.Strings(files)
	return files, nil
}

func parseOnboardingEnvironmentKeys(raw []byte) []string {
	var keys []string
	seen := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		for _, key := range onboardingEnvironmentKeysFromLine(scanner.Text()) {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func onboardingEnvironmentKeysFromLine(line string) []string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil
	}
	if index := strings.Index(line, "#"); index >= 0 {
		line = strings.TrimSpace(line[:index])
	}
	if strings.HasPrefix(line, "export ") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	}
	fields := strings.Fields(line)
	keys := make([]string, 0, len(fields))
	for _, field := range fields {
		key := field
		if before, _, ok := strings.Cut(field, "="); ok {
			key = before
		}
		key = strings.TrimSpace(key)
		if validOnboardingEnvironmentKey(key) {
			keys = append(keys, key)
		}
	}
	return keys
}

func validOnboardingEnvironmentKey(key string) bool {
	if key == "" {
		return false
	}
	for index, char := range key {
		switch {
		case char == '_':
		case char >= 'A' && char <= 'Z':
		case char >= 'a' && char <= 'z' && index == 0:
		case char >= 'a' && char <= 'z':
		case char >= '0' && char <= '9' && index > 0:
		default:
			return false
		}
	}
	first := key[0]
	return first == '_' || first >= 'A' && first <= 'Z' || first >= 'a' && first <= 'z'
}

func environmentWithoutKeys(keys []string) []string {
	remove := make(map[string]bool, len(keys))
	for _, key := range keys {
		remove[key] = true
	}
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && remove[key] {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func onboardingSanitizedGateCommand(command string, keys []string) string {
	parts := []string{"env"}
	for _, key := range keys {
		parts = append(parts, "-u", key)
	}
	prefix := strings.Join(parts, " ")
	if onboardingCommandNeedsShell(command) {
		return prefix + " sh -c " + onboardingShellQuote(command)
	}
	return prefix + " " + strings.TrimSpace(command)
}

func onboardingCommandNeedsShell(command string) bool {
	return strings.ContainsAny(command, "&;|<>$`'\"\n\r")
}

func onboardingShellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func writeOnboardingGateDiagnosticPretty(w io.Writer, result onboardingGateDiagnosticResult) error {
	lines := []string{
		"gate diagnostic: " + result.Status,
		"source_root: " + result.SourceRoot,
		"recommended_gate_command: " + result.RecommendedGateCommand,
	}
	if result.FailingCommand != "" {
		lines = append(lines, "failing_command: "+result.FailingCommand)
	}
	if result.PassingCommand != "" {
		lines = append(lines, "passing_command: "+result.PassingCommand)
	}
	if len(result.RelevantEnvironmentKeys) > 0 {
		lines = append(lines, "relevant_environment_keys: "+strings.Join(result.RelevantEnvironmentKeys, ","))
	}
	if result.PassingSanitizedCommand != "" {
		lines = append(lines, "passing_sanitized_command: "+result.PassingSanitizedCommand)
	}
	if len(result.CandidateEnvironmentFiles) > 0 {
		lines = append(lines, "candidate_environment_files: "+strings.Join(result.CandidateEnvironmentFiles, ","))
	}
	if result.Detail != "" {
		lines = append(lines, "detail: "+result.Detail)
	}
	_, err := fmt.Fprintln(w, strings.Join(lines, "\n"))
	return err
}
