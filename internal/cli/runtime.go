package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
)

const (
	defaultRuntimeEnv      = "prod"
	defaultRuntimeLogLevel = "info"
	runtimeCommandTimeout  = 5 * time.Second

	runtimeSourceFlag     = "flag"
	runtimeSourceConfig   = "config"
	runtimeSourceWorkflow = "workflow"
	runtimeSourceDefault  = "default"

	githubAuthHint = `Run gh auth login --scopes "repo,read:org,read:project,project" and set github_token: gh in global.yaml. For existing auth, run gh auth refresh -h github.com --scopes "repo,read:org,read:project,project".`
)

type RuntimeValue struct {
	Value  string
	Source string
}

type RuntimeIntValue struct {
	Value  int
	Source string
}

type RuntimeSecret struct {
	Value       string
	Source      string
	ResolvedVia string
	Required    bool
}

type RuntimeWarning struct {
	Name   string
	Detail string
	Hint   string
}

type RuntimeSettings struct {
	ConfigPath  RuntimeValue
	Env         RuntimeValue
	LogLevel    RuntimeValue
	Port        RuntimeIntValue
	GitHubToken RuntimeSecret
	Warnings    []RuntimeWarning
}

type runtimeInput struct {
	Config     *globalconfig.Config
	ConfigPath globalconfig.PathResolution
	Workflow   string
	Flags      runtimeFlags
}

type runtimeFlags struct {
	Env      runtimeStringFlag
	LogLevel runtimeStringFlag
	Port     runtimeIntFlag
}

type runtimeStringFlag struct {
	Value string
	Set   bool
}

type runtimeIntFlag struct {
	Value int
	Set   bool
}

type runtimeGitHubTokenState struct {
	mu    sync.RWMutex
	value string
}

type runtimeDeps struct {
	lookupEnv    func(string) string
	ghAuthToken  func(context.Context) (string, error)
	loadWorkflow func(string) (workflowconfig.Workflow, error)
}

func newRuntimeGitHubTokenState(value string) *runtimeGitHubTokenState {
	state := &runtimeGitHubTokenState{}
	state.set(value)
	return state
}

func (s *runtimeGitHubTokenState) get() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value
}

func (s *runtimeGitHubTokenState) set(value string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = strings.TrimSpace(value)
}

func resolveRuntimeSettings(ctx context.Context, input runtimeInput, deps runtimeDeps) (RuntimeSettings, error) {
	deps = deps.withDefaults()

	settings := RuntimeSettings{
		ConfigPath: RuntimeValue{
			Value:  input.ConfigPath.Path,
			Source: string(input.ConfigPath.Rule),
		},
		Env: resolveRuntimeString(runtimeStringInput{
			Flag:          input.Flags.Env,
			EnvCandidates: []string{"ENV", "DETENT_ENV"},
			ConfigValue:   configEnv(input.Config),
			DefaultValue:  defaultRuntimeEnv,
		}, deps.lookupEnv),
		LogLevel: resolveRuntimeString(runtimeStringInput{
			Flag:          input.Flags.LogLevel,
			EnvCandidates: []string{"LOG_LEVEL", "DETENT_LOG_LEVEL"},
			ConfigValue:   configLogLevel(input.Config),
			DefaultValue:  defaultRuntimeLogLevel,
		}, deps.lookupEnv),
	}

	port, err := resolveRuntimePort(ctx, input, deps)
	if err != nil {
		return RuntimeSettings{}, err
	}
	settings.Port = port

	token, warnings, err := resolveRuntimeGitHubToken(ctx, input.Config, deps)
	if err != nil {
		return RuntimeSettings{}, err
	}
	settings.GitHubToken = token
	settings.Warnings = warnings
	return settings, nil
}

type runtimeStringInput struct {
	Flag          runtimeStringFlag
	EnvCandidates []string
	ConfigValue   string
	DefaultValue  string
}

func resolveRuntimeString(input runtimeStringInput, lookupEnv func(string) string) RuntimeValue {
	if input.Flag.Set {
		if value := strings.TrimSpace(input.Flag.Value); value != "" {
			return RuntimeValue{Value: value, Source: runtimeSourceFlag}
		}
	}
	for _, key := range input.EnvCandidates {
		if value := strings.TrimSpace(lookupEnv(key)); value != "" {
			return RuntimeValue{Value: value, Source: key}
		}
	}
	if value := strings.TrimSpace(input.ConfigValue); value != "" {
		return RuntimeValue{Value: value, Source: runtimeSourceConfig}
	}
	return RuntimeValue{Value: input.DefaultValue, Source: runtimeSourceDefault}
}

func resolveRuntimePort(ctx context.Context, input runtimeInput, deps runtimeDeps) (RuntimeIntValue, error) {
	if input.Flags.Port.Set {
		if input.Flags.Port.Value < 0 {
			return RuntimeIntValue{}, WrapValidation(hintedError(nil, "--port must be greater than or equal to 0", exampleHint(portExampleCommand), portExampleCommand))
		}
		return RuntimeIntValue{Value: input.Flags.Port.Value, Source: runtimeSourceFlag}, nil
	}
	if raw := strings.TrimSpace(deps.lookupEnv("PORT")); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 0 {
			return RuntimeIntValue{}, WrapValidation(hintedError(nil, "PORT must be an integer greater than or equal to 0", "set PORT=4000 or run detent --port 0", portExampleCommand))
		}
		return RuntimeIntValue{Value: port, Source: "PORT"}, nil
	}
	if input.Config != nil && input.Config.Port != nil {
		return RuntimeIntValue{Value: *input.Config.Port, Source: runtimeSourceConfig}, nil
	}
	if input.Config != nil {
		if port, ok := globalWorkflowServerPort(ctx, *input.Config, deps); ok {
			return RuntimeIntValue{Value: port, Source: runtimeSourceWorkflow}, nil
		}
	}
	if port, ok := workflowServerPort(ctx, input.Workflow, deps); ok {
		return RuntimeIntValue{Value: port, Source: runtimeSourceWorkflow}, nil
	}
	return RuntimeIntValue{Value: defaultWebPort, Source: runtimeSourceDefault}, nil
}

func globalWorkflowServerPort(ctx context.Context, cfg globalconfig.Config, deps runtimeDeps) (int, bool) {
	if err := ctx.Err(); err != nil {
		return 0, false
	}
	project := firstGlobalProject(cfg)
	if strings.TrimSpace(project.Workflow) == "" {
		return 0, false
	}
	workflow, err := loadRuntimeProjectWorkflow(ctx, project, deps)
	if err != nil || workflow.Config.Server.Port == nil {
		return 0, false
	}
	return *workflow.Config.Server.Port, true
}

func workflowServerPort(ctx context.Context, path string, deps runtimeDeps) (int, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0, false
	}
	if err := ctx.Err(); err != nil {
		return 0, false
	}
	workflow, err := deps.loadWorkflow(path)
	if err != nil || workflow.Config.Server.Port == nil {
		return 0, false
	}
	return *workflow.Config.Server.Port, true
}

func resolveRuntimeGitHubToken(ctx context.Context, cfg *globalconfig.Config, deps runtimeDeps) (RuntimeSecret, []RuntimeWarning, error) {
	if token := strings.TrimSpace(deps.lookupEnv("GITHUB_TOKEN")); token != "" {
		return RuntimeSecret{Value: token, Source: "GITHUB_TOKEN", Required: true}, nil, nil
	}

	token, requiresRuntimeToken := trackerGitHubToken(ctx, cfg, deps)
	if token.Value == "" && !requiresRuntimeToken {
		return RuntimeSecret{Required: false}, nil, nil
	}

	if cfg != nil {
		if configuredToken := strings.TrimSpace(cfg.GitHubToken); configuredToken != "" {
			resolved, err := resolveConfiguredGitHubToken(ctx, configuredToken, deps)
			if err != nil {
				return RuntimeSecret{}, nil, err
			}
			warnings := literalTokenWarnings(configuredToken)
			return resolved, warnings, nil
		}
	}

	if token.Value != "" {
		token.Required = true
		return token, nil, nil
	}
	if requiresRuntimeToken {
		return RuntimeSecret{}, nil, GitHubAuthError(hintedError(nil, "GITHUB_TOKEN is not set, github_token is not configured, and no usable tracker.api_key was found", githubAuthHint, ghAuthLoginCommand))
	}
	return RuntimeSecret{Required: false}, nil, nil
}

func resolveConfiguredGitHubToken(ctx context.Context, token string, deps runtimeDeps) (RuntimeSecret, error) {
	if githubTokenSentinel(token) {
		resolved, err := deps.ghAuthToken(ctx)
		if err != nil {
			cause := fmt.Errorf("resolve github_token via gh auth token: %w", err)
			return RuntimeSecret{}, GitHubAuthError(hintedError(cause, cause.Error(), githubAuthHint, ghAuthLoginCommand))
		}
		resolved = strings.TrimSpace(resolved)
		if resolved == "" {
			message := "resolve github_token via gh auth token: empty token"
			return RuntimeSecret{}, GitHubAuthError(hintedError(nil, message, githubAuthHint, ghAuthLoginCommand))
		}
		return RuntimeSecret{Value: resolved, Source: "github_token", ResolvedVia: "gh", Required: true}, nil
	}
	return RuntimeSecret{Value: strings.TrimSpace(token), Source: "github_token", Required: true}, nil
}

func trackerGitHubToken(ctx context.Context, cfg *globalconfig.Config, deps runtimeDeps) (RuntimeSecret, bool) {
	if cfg == nil {
		return RuntimeSecret{}, false
	}

	requiresRuntimeToken := false
	for _, project := range cfg.Projects {
		workflow, err := loadRuntimeProjectWorkflow(ctx, project, deps)
		if err != nil || workflow.Config.Tracker.Kind != workflowconfig.TrackerGitHub {
			continue
		}
		if trackerHasGitHubAppCredentials(workflow.Config.Tracker, deps.lookupEnv) {
			continue
		}
		if token, source := resolveRuntimeSecret(workflow.Config.Tracker.APIKey, deps.lookupEnv); token != "" {
			if source == "" {
				source = "tracker.api_key"
			}
			return RuntimeSecret{Value: token, Source: source}, true
		}
		requiresRuntimeToken = true
	}
	return RuntimeSecret{}, requiresRuntimeToken
}

func loadRuntimeProjectWorkflow(ctx context.Context, project globalconfig.Project, deps runtimeDeps) (workflowconfig.Workflow, error) {
	if strings.TrimSpace(project.WorkflowRef) == "" {
		return deps.loadWorkflow(project.Workflow)
	}
	return projectpkg.LoadWorkflowContext(ctx, project)
}

func trackerHasGitHubAppCredentials(tracker workflowconfig.Tracker, lookupEnv func(string) string) bool {
	return resolveRuntimeSecretValue(tracker.GitHubAppID, lookupEnv) != "" &&
		resolveRuntimeSecretValue(tracker.GitHubAppInstallationID, lookupEnv) != "" &&
		(resolveRuntimeSecretValue(tracker.GitHubAppPrivateKey, lookupEnv) != "" ||
			resolveRuntimeSecretValue(tracker.GitHubAppPrivateKeyPath, lookupEnv) != "")
}

func resolveRuntimeSecret(value string, lookupEnv func(string) string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	if after, ok := strings.CutPrefix(value, "$"); ok {
		name := after
		if validEnvName(name) {
			return strings.TrimSpace(lookupEnv(name)), name
		}
	}
	return value, "tracker.api_key"
}

func resolveRuntimeSecretValue(value string, lookupEnv func(string) string) string {
	resolved, _ := resolveRuntimeSecret(value, lookupEnv)
	return resolved
}

func githubTokenSentinel(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "gh", "gh-auth", "${gh auth token}", "$(gh auth token)":
		return true
	default:
		return false
	}
}

func literalTokenWarnings(token string) []RuntimeWarning {
	if githubTokenSentinel(token) || !looksLikeGitHubToken(token) {
		return nil
	}
	return []RuntimeWarning{{
		Name:   "GitHub token storage",
		Detail: "github_token is a literal token in global.yaml",
		Hint:   "Use github_token: gh to resolve the token from GitHub CLI at startup.",
	}}
}

func looksLikeGitHubToken(token string) bool {
	token = strings.TrimSpace(token)
	for _, prefix := range []string{"ghp_", "github_pat_", "gho_", "ghu_", "ghs_", "ghr_"} {
		if strings.HasPrefix(token, prefix) {
			return true
		}
	}
	return false
}

func runtimeSettingsDetail(settings RuntimeSettings) string {
	parts := []string{
		runtimeTextDetail("config_path", settings.ConfigPath.Value, settings.ConfigPath.Source),
		runtimeTextDetail("env", settings.Env.Value, settings.Env.Source),
		runtimeTextDetail("log_level", settings.LogLevel.Value, settings.LogLevel.Source),
		runtimeIntDetail("port", settings.Port.Value, settings.Port.Source),
		runtimeGitHubTokenDetail(settings.GitHubToken),
	}
	return strings.Join(parts, "; ")
}

func runtimeTextDetail(name string, value string, source string) string {
	return fmt.Sprintf("%s=%s (%s)", name, value, sourceDetail(source))
}

func runtimeIntDetail(name string, value int, source string) string {
	return fmt.Sprintf("%s=%d (%s)", name, value, sourceDetail(source))
}

func runtimeGitHubTokenDetail(token RuntimeSecret) string {
	if token.Value == "" {
		if token.Required {
			return "github_token=unresolved"
		}
		return "github_token=not required"
	}
	if token.ResolvedVia == "gh" {
		return "github_token=resolved via gh"
	}
	return fmt.Sprintf("github_token=redacted (%s)", sourceDetail(token.Source))
}

func runtimeGlobalGitHubToken(token RuntimeSecret) string {
	switch strings.TrimSpace(token.Source) {
	case "GITHUB_TOKEN", "github_token":
		return strings.TrimSpace(token.Value)
	default:
		return ""
	}
}

func runtimeGitHubTokenVersion(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
}

func sourceDetail(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "unknown"
	}
	return source
}

func configEnv(cfg *globalconfig.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.Env
}

func configLogLevel(cfg *globalconfig.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.LogLevel
}

func (d runtimeDeps) withDefaults() runtimeDeps {
	if d.lookupEnv == nil {
		d.lookupEnv = os.Getenv
	}
	if d.ghAuthToken == nil {
		d.ghAuthToken = defaultGHAuthToken
	}
	if d.loadWorkflow == nil {
		d.loadWorkflow = workflowconfig.LoadWorkflow
	}
	return d
}

func runtimeDepsFromOptions(opts options) runtimeDeps {
	return runtimeDeps{
		lookupEnv:   opts.lookupEnv,
		ghAuthToken: opts.ghAuthToken,
	}
}

func defaultGHAuthToken(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path, err := exec.LookPath("gh")
	if err != nil {
		return "", errors.New("gh was not found on PATH")
	}

	commandCtx, cancel := context.WithTimeout(ctx, runtimeCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(commandCtx, path, "auth", "token") // #nosec G204 -- gh path is PATH-resolved and arguments are fixed.
	output, err := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		return "", commandCtx.Err()
	}
	if err != nil {
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return "", fmt.Errorf("gh auth token failed: %w: %s", err, detail)
		}
		return "", fmt.Errorf("gh auth token failed: %w", err)
	}
	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", errors.New("gh auth token returned an empty token")
	}
	return token, nil
}
