package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBehaviorEvidence(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		data string
		want bool
	}{
		{"executed", `{"Action":"pass","Test":"TestGuard"}`, false},
		{"no tests", `{"Action":"pass"}`, true},
		{"deleted test", `{"Action":"pass","Test":"TestOther"}`, true},
		{"skipped", `{"Action":"skip","Test":"TestGuard"}`, true},
		{"skipped subtest", "{\"Action\":\"skip\",\"Test\":\"TestGuard/restart\"}\n{\"Action\":\"pass\",\"Test\":\"TestGuard\"}", true},
		{"failure", `{"Action":"fail","Test":"TestGuard"}`, true},
		{"missing evidence", "", true},
		{"malformed", "not json", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := verifyTestEvents([]byte(tt.data), []string{"TestGuard"}); (err != nil) != tt.want {
				t.Fatalf("verifyTestEvents() = %v, want error %v", err, tt.want)
			}
		})
	}
}

func TestBehaviorCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("repository behavior-command subprocess integration")
	}

	for _, tt := range []struct {
		name, body string
		want       int
	}{
		{"passing", "", 0}, {"failing", `t.Fatal("failure")`, 1}, {"skipped", `t.Skip("skip")`, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for name, data := range map[string]string{
				"go.mod":        "module example\n\ngo 1.26\n",
				"guard_test.go": "package example\nimport \"testing\"\nfunc TestGuard(t *testing.T){" + tt.body + "}\n",
				"policy.json":   `{"tests":{"./":["TestGuard"]}}`,
			} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(root)
			var out, errout bytes.Buffer
			if got := run(nil, &out, &errout); got == 0 {
				t.Fatal("default missing manifest passed")
			}
			if got := run([]string{"-policy", "policy.json"}, &out, &errout); got != tt.want {
				t.Fatalf("code %d want %d: %s", got, tt.want, &errout)
			}
		})
	}
}

func TestManifestValidation(t *testing.T) {
	t.Parallel()
	for _, data := range []string{`{}`, `{"tests":{}}`, `{"unknown":true}`, `{"tests":{"bad":["TestGuard"]}}`, `{"tests":{"./":[]}}`, `{"tests":{"./":["Other"]}}`, `{"tests":{"./":["TestGuard"],"./":["TestOther"]}}`, `{"tests":[]}`, `broken`} {
		t.Run(data, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readPolicy(path); err == nil {
				t.Fatal("invalid manifest passed")
			}
		})
	}
	var out bytes.Buffer
	if got := run([]string{"-unknown"}, &out, &out); got != 2 {
		t.Fatal(got)
	}
	if got := run([]string{"extra"}, &out, &out); got != 2 {
		t.Fatal(got)
	}
}

func TestManifestReferences(t *testing.T) {
	for _, tt := range []struct {
		name     string
		file     string
		source   string
		wantCode int
	}{
		{"declared without execution", "guard_test.go", "func TestGuard(t *testing.T) { t.Fatal(\"must not execute\") }", 0},
		{"renamed", "guard_test.go", "func TestRenamed(t *testing.T) {}", 1},
		{"production file", "guard.go", "func TestGuard(t *testing.T) {}", 1},
		{"method", "guard_test.go", "type guard struct{}\nfunc (guard) TestGuard(t *testing.T) {}", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for name, data := range map[string]string{
				tt.file:       "package example\nimport \"testing\"\n" + tt.source + "\n",
				"policy.json": `{"tests":{"./":["TestGuard"]}}`,
			} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(root)
			var out, errout bytes.Buffer
			if got := run([]string{"-policy", "policy.json", "-check-manifest"}, &out, &errout); got != tt.wantCode {
				t.Fatalf("code %d want %d: %s", got, tt.wantCode, &errout)
			}
			if tt.wantCode != 0 && !strings.Contains(errout.String(), `required behavioral test "TestGuard" is not declared`) {
				t.Fatalf("missing reference diagnostic: %s", &errout)
			}
		})
	}
}
