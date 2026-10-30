package cli

import (
	"bytes"
	"encoding/json"
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
	t.Setenv(gotestreceipt.TokenEnvironment, "receipt-token-sentinel")
	var mu sync.Mutex
	var receipts []gotestreceipt.Receipt
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/go-test-receipts/executions") {
			w.WriteHeader(http.StatusCreated)
			if err := json.NewEncoder(w).Encode(gotestreceipt.Execution{ID: "execution", DigestKey: []byte(strings.Repeat("k", 32))}); err != nil {
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
		{name: "second checkout reuses", root: second, want: "1 of 1 test binaries reused, 0 recorded"},
		{name: "changed input executes and fails", root: broken, want: "0 of 1 test binaries reused, 0 recorded", wantErr: true},
	} {
		output, err := run(step.root)
		if strings.Contains(output, gotestreceipt.TokenEnvironment+"=") || strings.Contains(output, strings.Repeat("k", 32)) {
			t.Fatal("validation script received receipt credentials")
		}
		if (err != nil) != step.wantErr || !strings.Contains(output, step.want) {
			t.Fatalf("%s: err = %v, output missing %q:\n%s", step.name, err, step.want, output)
		}
	}
}
