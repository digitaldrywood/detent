package main

import (
	"bytes"
	"os"
	"path/filepath"
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
	for _, data := range []string{`{}`, `{"tests":{}}`, `{"unknown":true}`, `{"tests":{"bad":["TestGuard"]}}`, `{"tests":{"./":[]}}`, `{"tests":{"./":["Other"]}}`, `broken`} {
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
