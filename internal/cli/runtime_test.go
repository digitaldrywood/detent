package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestResolveRuntimeSettingsPrecedence(t *testing.T) {
	t.Parallel()

	configPort := 4100
	tests := []struct {
		name         string
		flags        runtimeFlags
		env          map[string]string
		cfg          *globalconfig.Config
		wantEnv      RuntimeValue
		wantLogLevel RuntimeValue
		wantPort     RuntimeIntValue
	}{
		{
			name: "flags win",
			flags: runtimeFlags{
				Env:      runtimeStringFlag{Value: "flag-env", Set: true},
				LogLevel: runtimeStringFlag{Value: "debug", Set: true},
				Port:     runtimeIntFlag{Value: 0, Set: true},
			},
			env: map[string]string{
				"ENV":       "env-env",
				"LOG_LEVEL": "error",
				"PORT":      "4200",
			},
			cfg:          &globalconfig.Config{Env: "config-env", LogLevel: "warn", Port: &configPort},
			wantEnv:      RuntimeValue{Value: "flag-env", Source: runtimeSourceFlag},
			wantLogLevel: RuntimeValue{Value: "debug", Source: runtimeSourceFlag},
			wantPort:     RuntimeIntValue{Value: 0, Source: runtimeSourceFlag},
		},
		{
			name: "env wins after flags",
			env: map[string]string{
				"ENV":       "env-env",
				"LOG_LEVEL": "error",
				"PORT":      "4200",
			},
			cfg:          &globalconfig.Config{Env: "config-env", LogLevel: "warn", Port: &configPort},
			wantEnv:      RuntimeValue{Value: "env-env", Source: "ENV"},
			wantLogLevel: RuntimeValue{Value: "error", Source: "LOG_LEVEL"},
			wantPort:     RuntimeIntValue{Value: 4200, Source: "PORT"},
		},
		{
			name: "deprecated env fallbacks win before config",
			env: map[string]string{
				"DETENT_ENV":       "detent-env",
				"DETENT_LOG_LEVEL": "debug",
			},
			cfg:          &globalconfig.Config{Env: "config-env", LogLevel: "warn", Port: &configPort},
			wantEnv:      RuntimeValue{Value: "detent-env", Source: "DETENT_ENV"},
			wantLogLevel: RuntimeValue{Value: "debug", Source: "DETENT_LOG_LEVEL"},
			wantPort:     RuntimeIntValue{Value: 4100, Source: runtimeSourceConfig},
		},
		{
			name:         "config wins before defaults",
			cfg:          &globalconfig.Config{Env: "config-env", LogLevel: "warn", Port: &configPort},
			wantEnv:      RuntimeValue{Value: "config-env", Source: runtimeSourceConfig},
			wantLogLevel: RuntimeValue{Value: "warn", Source: runtimeSourceConfig},
			wantPort:     RuntimeIntValue{Value: 4100, Source: runtimeSourceConfig},
		},
		{
			name:         "defaults are last",
			cfg:          &globalconfig.Config{},
			wantEnv:      RuntimeValue{Value: defaultRuntimeEnv, Source: runtimeSourceDefault},
			wantLogLevel: RuntimeValue{Value: defaultRuntimeLogLevel, Source: runtimeSourceDefault},
			wantPort:     RuntimeIntValue{Value: defaultWebPort, Source: runtimeSourceDefault},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveRuntimeSettings(context.Background(), runtimeInput{
				Config: tt.cfg,
				Flags:  tt.flags,
			}, runtimeDeps{
				lookupEnv: mapLookup(tt.env),
			})
			if err != nil {
				t.Fatalf("resolveRuntimeSettings() error = %v", err)
			}

			if got.Env != tt.wantEnv {
				t.Fatalf("Env = %#v, want %#v", got.Env, tt.wantEnv)
			}
			if got.LogLevel != tt.wantLogLevel {
				t.Fatalf("LogLevel = %#v, want %#v", got.LogLevel, tt.wantLogLevel)
			}
			if got.Port != tt.wantPort {
				t.Fatalf("Port = %#v, want %#v", got.Port, tt.wantPort)
			}
		})
	}
}

func TestResolveRuntimeSettingsGitHubTokenPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		env         map[string]string
		cfg         globalconfig.Config
		workflow    workflowconfig.Config
		ghToken     string
		wantToken   string
		wantSource  string
		wantVia     string
		wantGhCalls int
	}{
		{
			name: "environment wins before config and tracker",
			env: map[string]string{
				"GITHUB_TOKEN":  "env-token",
				"PROJECT_TOKEN": "tracker-token",
			},
			cfg:         githubRuntimeConfig("gh"),
			workflow:    githubWorkflow("$PROJECT_TOKEN"),
			ghToken:     "gh-token",
			wantToken:   "env-token",
			wantSource:  "GITHUB_TOKEN",
			wantGhCalls: 0,
		},
		{
			name:        "config literal wins before tracker",
			env:         map[string]string{"PROJECT_TOKEN": "tracker-token"},
			cfg:         githubRuntimeConfig("config-token"),
			workflow:    githubWorkflow("$PROJECT_TOKEN"),
			wantToken:   "config-token",
			wantSource:  "github_token",
			wantGhCalls: 0,
		},
		{
			name:        "config sentinel resolves through gh auth token",
			cfg:         githubRuntimeConfig("${gh auth token}"),
			workflow:    githubWorkflow("tracker-token"),
			ghToken:     "gh-token",
			wantToken:   "gh-token",
			wantSource:  "github_token",
			wantVia:     "gh",
			wantGhCalls: 1,
		},
		{
			name:        "tracker api key is final fallback",
			env:         map[string]string{"PROJECT_TOKEN": "tracker-token"},
			cfg:         githubRuntimeConfig(""),
			workflow:    githubWorkflow("$PROJECT_TOKEN"),
			wantToken:   "tracker-token",
			wantSource:  "PROJECT_TOKEN",
			wantGhCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ghCalls := 0
			got, err := resolveRuntimeSettings(context.Background(), runtimeInput{
				Config: &tt.cfg,
			}, runtimeDeps{
				lookupEnv: mapLookup(tt.env),
				ghAuthToken: func(context.Context) (string, error) {
					ghCalls++
					return tt.ghToken, nil
				},
				loadWorkflow: func(string) (workflowconfig.Workflow, error) {
					return workflowconfig.Workflow{Config: tt.workflow}, nil
				},
			})
			if err != nil {
				t.Fatalf("resolveRuntimeSettings() error = %v", err)
			}
			if got.GitHubToken.Value != tt.wantToken {
				t.Fatalf("GitHubToken.Value = %q, want %q", got.GitHubToken.Value, tt.wantToken)
			}
			if got.GitHubToken.Source != tt.wantSource {
				t.Fatalf("GitHubToken.Source = %q, want %q", got.GitHubToken.Source, tt.wantSource)
			}
			if got.GitHubToken.ResolvedVia != tt.wantVia {
				t.Fatalf("GitHubToken.ResolvedVia = %q, want %q", got.GitHubToken.ResolvedVia, tt.wantVia)
			}
			if ghCalls != tt.wantGhCalls {
				t.Fatalf("gh calls = %d, want %d", ghCalls, tt.wantGhCalls)
			}
		})
	}
}

func TestResolveRuntimeSettingsGitHubTokenErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cfg      globalconfig.Config
		ghErr    error
		wantErr  string
		wantHint string
	}{
		{
			name:     "sentinel failure includes gh auth guidance",
			cfg:      githubRuntimeConfig("gh"),
			ghErr:    errors.New("not logged in"),
			wantErr:  "not logged in",
			wantHint: "gh auth login",
		},
		{
			name:     "github projects require token",
			cfg:      githubRuntimeConfig(""),
			wantErr:  "GITHUB_TOKEN",
			wantHint: "gh auth login",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := resolveRuntimeSettings(context.Background(), runtimeInput{
				Config: &tt.cfg,
			}, runtimeDeps{
				lookupEnv: mapLookup(nil),
				ghAuthToken: func(context.Context) (string, error) {
					return "", tt.ghErr
				},
				loadWorkflow: func(string) (workflowconfig.Workflow, error) {
					return workflowconfig.Workflow{Config: githubWorkflow("")}, nil
				},
			})
			if err == nil {
				t.Fatal("resolveRuntimeSettings() error = nil, want error")
			}
			if !errors.Is(err, ErrGitHubAuth) {
				t.Fatalf("resolveRuntimeSettings() error = %v, want %v", err, ErrGitHubAuth)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			hint, _, ok := HintFor(err)
			if !ok {
				t.Fatalf("HintFor(%v) ok = false, want true", err)
			}
			if !strings.Contains(hint, tt.wantHint) {
				t.Fatalf("hint = %q, want containing %q", hint, tt.wantHint)
			}
		})
	}
}

func TestResolveRuntimeSettingsDoesNotRequireTokenForGitHubApp(t *testing.T) {
	t.Parallel()

	got, err := resolveRuntimeSettings(context.Background(), runtimeInput{
		Config: ptrGlobalConfig(githubRuntimeConfig("")),
	}, runtimeDeps{
		lookupEnv: mapLookup(map[string]string{
			"APP_ID":           "12345",
			"INSTALLATION_ID":  "67890",
			"PRIVATE_KEY_PATH": ".detent/github-app.pem",
		}),
		loadWorkflow: func(string) (workflowconfig.Workflow, error) {
			return workflowconfig.Workflow{Config: githubAppWorkflow()}, nil
		},
	})
	if err != nil {
		t.Fatalf("resolveRuntimeSettings() error = %v", err)
	}
	if got.GitHubToken.Required {
		t.Fatalf("GitHubToken.Required = true, want false")
	}
	if got.GitHubToken.Value != "" {
		t.Fatalf("GitHubToken.Value = %q, want empty", got.GitHubToken.Value)
	}
}

func TestResolveRuntimeSettingsSkipsConfigTokenWhenNoProjectNeedsRuntimeToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		workflow workflowconfig.Config
		env      map[string]string
	}{
		{
			name:     "non github tracker",
			workflow: nonGitHubWorkflow(),
		},
		{
			name:     "github app tracker",
			workflow: githubAppWorkflow(),
			env: map[string]string{
				"APP_ID":           "12345",
				"INSTALLATION_ID":  "67890",
				"PRIVATE_KEY_PATH": ".detent/github-app.pem",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ghCalls := 0
			got, err := resolveRuntimeSettings(context.Background(), runtimeInput{
				Config: ptrGlobalConfig(githubRuntimeConfig("gh")),
			}, runtimeDeps{
				lookupEnv: mapLookup(tt.env),
				ghAuthToken: func(context.Context) (string, error) {
					ghCalls++
					return "", errors.New("gh should not be called")
				},
				loadWorkflow: func(string) (workflowconfig.Workflow, error) {
					return workflowconfig.Workflow{Config: tt.workflow}, nil
				},
			})
			if err != nil {
				t.Fatalf("resolveRuntimeSettings() error = %v", err)
			}
			if ghCalls != 0 {
				t.Fatalf("gh calls = %d, want 0", ghCalls)
			}
			if got.GitHubToken.Required {
				t.Fatalf("GitHubToken.Required = true, want false")
			}
			if got.GitHubToken.Value != "" {
				t.Fatalf("GitHubToken.Value = %q, want empty", got.GitHubToken.Value)
			}
		})
	}
}

func TestResolveRuntimeSettingsRequiresTokenForMissingGitHubAppEnvRefs(t *testing.T) {
	t.Parallel()

	_, err := resolveRuntimeSettings(context.Background(), runtimeInput{
		Config: ptrGlobalConfig(githubRuntimeConfig("")),
	}, runtimeDeps{
		lookupEnv: mapLookup(nil),
		loadWorkflow: func(string) (workflowconfig.Workflow, error) {
			return workflowconfig.Workflow{Config: githubAppWorkflow()}, nil
		},
	})
	if err == nil {
		t.Fatal("resolveRuntimeSettings() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("error = %v, want containing GITHUB_TOKEN", err)
	}
}

func TestRuntimeSettingsDetailRedactsGitHubToken(t *testing.T) {
	t.Parallel()

	settings := RuntimeSettings{
		ConfigPath:  RuntimeValue{Value: "/tmp/global.yaml", Source: string(globalconfig.PathRuleFlag)},
		Env:         RuntimeValue{Value: "dev", Source: "ENV"},
		LogLevel:    RuntimeValue{Value: "debug", Source: "LOG_LEVEL"},
		Port:        RuntimeIntValue{Value: 4000, Source: runtimeSourceConfig},
		GitHubToken: RuntimeSecret{Value: "ghp_secret", Source: "GITHUB_TOKEN"},
	}

	detail := runtimeSettingsDetail(settings)
	if strings.Contains(detail, "ghp_secret") {
		t.Fatalf("runtime detail leaked token: %s", detail)
	}
	for _, want := range []string{
		"config_path=/tmp/global.yaml (--config)",
		"env=dev (ENV)",
		"log_level=debug (LOG_LEVEL)",
		"port=4000 (config)",
		"github_token=redacted (GITHUB_TOKEN)",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("runtime detail missing %q:\n%s", want, detail)
		}
	}

	settings.GitHubToken.ResolvedVia = "gh"
	detail = runtimeSettingsDetail(settings)
	if !strings.Contains(detail, "github_token=resolved via gh") {
		t.Fatalf("runtime detail missing gh sentinel source:\n%s", detail)
	}
}

func TestRuntimeGlobalGitHubTokenSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		token RuntimeSecret
		want  string
	}{
		{
			name:  "environment token is global",
			token: RuntimeSecret{Value: "env-token", Source: "GITHUB_TOKEN"},
			want:  "env-token",
		},
		{
			name:  "config token is global",
			token: RuntimeSecret{Value: "config-token", Source: "github_token"},
			want:  "config-token",
		},
		{
			name:  "literal tracker token stays workflow local",
			token: RuntimeSecret{Value: "tracker-token", Source: "tracker.api_key"},
			want:  "",
		},
		{
			name:  "tracker env token stays workflow local",
			token: RuntimeSecret{Value: "tracker-token", Source: "PROJECT_TOKEN"},
			want:  "",
		},
		{
			name:  "empty token",
			token: RuntimeSecret{},
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := runtimeGlobalGitHubToken(tt.token); got != tt.want {
				t.Fatalf("runtimeGlobalGitHubToken() = %q, want %q", got, tt.want)
			}
		})
	}
}

func githubRuntimeConfig(token string) globalconfig.Config {
	return globalconfig.Config{
		GitHubToken: token,
		Projects: []globalconfig.Project{
			{ID: "detent", Workflow: "WORKFLOW.md", Workdir: ".", Weight: 1},
		},
	}
}

func githubWorkflow(apiKey string) workflowconfig.Config {
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerGitHub
	cfg.Tracker.APIKey = apiKey
	return cfg
}

func nonGitHubWorkflow() workflowconfig.Config {
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	return cfg
}

func githubAppWorkflow() workflowconfig.Config {
	cfg := githubWorkflow("")
	cfg.Tracker.GitHubAppID = "$APP_ID"
	cfg.Tracker.GitHubAppInstallationID = "$INSTALLATION_ID"
	cfg.Tracker.GitHubAppPrivateKeyPath = "$PRIVATE_KEY_PATH"
	return cfg
}

func ptrGlobalConfig(cfg globalconfig.Config) *globalconfig.Config {
	return &cfg
}

func mapLookup(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}
