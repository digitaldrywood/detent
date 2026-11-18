package cli

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateCommandFiltersOperatorEnvironment(t *testing.T) {
	command := "env"
	if runtime.GOOS == "windows" {
		command = "set"
	}
	for _, tc := range []struct {
		name    string
		present bool
		value   string
	}{
		{"GH_TOKEN", false, ""},
		{"DETENT_HUB_TOKEN", false, ""},
		{"OPENAI_API_KEY", false, ""},
		{"ANTHROPIC_API_KEY", false, ""},
		{"AWS_SECRET_ACCESS_KEY", false, ""},
		{"NPM_TOKEN", false, ""},
		{"npm_config_//registry.npmjs.org/:_authToken", false, ""},
		{"GO_PRIVATE_TOKEN", false, ""},
		{"NODE_AUTH_TOKEN", false, ""},
		{"BASH_ENV", false, ""},
		{"ENV", false, ""},
		{"NODE_OPTIONS", false, ""},
		{"GIT_CONFIG_COUNT", false, ""},
		{"SSH_AUTH_SOCK", false, ""},
		{"UNDECLARED_VARIABLE", false, ""},
		{"GOROOT", true, ""},
		{"GOPATH", true, ""},
		{"GOCACHE", true, ""},
		{"GOTOOLCHAIN", true, ""},
		{"GOMODCACHE", true, ""},
		{"GOMAXPROCS", true, ""},
		{"CGO_ENABLED", true, ""},
		{"NODE_PATH", true, ""},
		{"NPM_CONFIG_CACHE", true, ""},
		{"PLAYWRIGHT_BROWSERS_PATH", true, ""},
		{"TMPDIR", true, ""},
		{"GOPROXY", true, "https://proxy.example,direct"},
		{"GOPROXY", true, "https://first.example|https://second.example,direct"},
		{"GOPROXY", true, "direct"},
		{"GOPROXY", true, "off"},
		{"GOPROXY", false, "https://operator:fixture-password@proxy.example"},
		{"GOPROXY", false, "https://fixture-token@proxy.example,direct"},
		{"GOPROXY", false, "https://public.example|https://operator:fixture-password@private.example,direct"},
		{"GOPROXY", false, "operator:fixture-password@proxy.example"},
		{"GOPROXY", false, "https://operator:fixture%40password@proxy.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			value := tc.value
			if value == "" {
				value = filepath.Join(root, "fixture-value")
				if tc.name == "TMPDIR" {
					if err := os.Mkdir(value, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			}
			t.Setenv(tc.name, value)
			var output bytes.Buffer
			cmd := newValidateCommand(func(key string) string {
				if key == "DETENT_HUB_TOKEN" {
					return "test-token"
				}
				return ""
			}, &http.Client{Transport: readinessRoundTripper(func(*http.Request) (*http.Response, error) {
				t.Error("environment command must not access the Hub")
				return nil, errors.New("unexpected Hub request")
			})}, func() (string, error) { return "fixture-wrapper", nil })
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs([]string{"--project", "prj_cli", "--organization", "org_cli", "--hub-url", "https://cloud.example", "--dir", root, command})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			env := make(map[string]string)
			for line := range strings.SplitSeq(output.String(), "\n") {
				name, value, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "=")
				if ok {
					env[name] = value
				}
			}
			got, present := env[tc.name]
			if present != tc.present || (tc.present && got != value) {
				t.Fatalf("variable presence = %v, want %v; preserved value = %v", present, tc.present, got == value)
			}
			for _, name := range []string{"PATH", "HOME"} {
				if value, ok := os.LookupEnv(name); ok && env[name] != value {
					t.Errorf("%s was not preserved", name)
				}
			}
			if os.Getenv(tc.name) != value {
				t.Fatal("validation changed the parent environment")
			}
		})
	}
}
