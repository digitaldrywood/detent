package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
)

func TestValidateCommandReusesGoTestReceiptsAcrossCheckouts(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess Go toolchain fixture")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Go command fixture")
	}
	var mu sync.Mutex
	var receipts []gotestreceipt.Receipt
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/api/v2/organizations/org_cli/projects/prj_cli/go-test-receipts/attestations" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			if err := json.NewEncoder(w).Encode(gotestreceipt.Attestation{ID: "attestation", DigestKey: []byte(strings.Repeat("k", gotestreceipt.DigestKeyBytes))}); err != nil {
				t.Error(err)
			}
			return
		}
		if r.URL.Path != "/api/v2/organizations/org_cli/projects/prj_cli/go-test-receipts" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPost {
			var receipt gotestreceipt.Receipt
			if err := json.NewDecoder(r.Body).Decode(&receipt); err != nil {
				t.Error(err)
			}
			if receipt.Attestation != "attestation" {
				t.Errorf("receipt attestation = %q, want the session's Hub attestation", receipt.Attestation)
			}
			receipts = append(receipts, receipt)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		candidates := []gotestreceipt.Candidate{}
		for _, receipt := range receipts {
			if receipt.Key == r.URL.Query().Get("key") {
				candidates = append(candidates, gotestreceipt.Candidate{Inputs: receipt.Inputs, Digest: receipt.Digest})
			}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"candidates": candidates}); err != nil {
			t.Error(err)
		}
	})
	httpClient := &http.Client{Transport: readinessRoundTripper(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Result(), nil
	})}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := gotestreceipt.Install(t.TempDir(), executable)
	if err != nil {
		t.Fatal(err)
	}
	checkout := func() string {
		root := t.TempDir()
		for name, content := range map[string]string{
			"go.mod":          "module example.test/validate\n\ngo 1.26.0\n",
			"pkg/input.txt":   "ok",
			"pkg/pkg_test.go": "package pkg\nimport (\"os\"; \"testing\")\nfunc TestInput(t *testing.T) {\n data, err := os.ReadFile(\"input.txt\")\n if err != nil || string(data) != \"ok\" { t.Fatalf(\"input %q %v\", data, err) }\n}\n",
		} {
			path := filepath.Join(root, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	run := func(root string) (string, error) {
		var output bytes.Buffer
		cmd := newValidateCommand(func(key string) string {
			if key == "DETENT_HUB_TOKEN" {
				return "test-token"
			}
			return ""
		}, httpClient, func() (string, error) { return wrapper, nil })
		cmd.SilenceUsage = true
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs([]string{"--project", "prj_cli", "--organization", "org_cli", "--hub-url", "https://cloud.example", "--dir", root, "env; CGO_ENABLED=0 go test ./..."})
		err := cmd.Execute()
		return output.String(), err
	}

	first, second, broken := checkout(), checkout(), checkout()
	if err := os.WriteFile(filepath.Join(broken, "pkg", "input.txt"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		name, root, want string
		wantErr          bool
	}{
		{name: "first checkout records", root: first, want: "0 of 1 test binaries reused, 1 recorded"},
		{name: "same checkout reuses", root: first, want: "1 of 1 test binaries reused, 0 recorded"},
		{name: "second checkout reuses", root: second, want: "1 of 1 test binaries reused, 0 recorded"},
		{name: "changed input executes and fails", root: broken, want: "0 of 1 test binaries reused, 0 recorded", wantErr: true},
	} {
		output, err := run(step.root)
		if strings.Contains(output, strings.Repeat("k", gotestreceipt.DigestKeyBytes)) {
			t.Fatal("validation script received the receipt digest key")
		}
		if (err != nil) != step.wantErr || !strings.Contains(output, step.want) {
			t.Fatalf("%s: err = %v, output missing %q:\n%s", step.name, err, step.want, output)
		}
	}
}

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
