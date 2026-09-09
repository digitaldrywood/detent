package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseEvidence(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	for _, tt := range []struct {
		name       string
		status     string
		conclusion string
		sha        string
		app        int64
		missing    bool
		wantError  bool
	}{
		{"success", "completed", "success", sha, 15368, false, false},
		{"stale commit", "completed", "success", strings.Repeat("b", 40), 15368, false, true},
		{"missing", "completed", "success", sha, 15368, true, true},
		{"cancelled", "completed", "cancelled", sha, 15368, false, true},
		{"skipped", "completed", "skipped", sha, 15368, false, true},
		{"neutral", "completed", "neutral", sha, 15368, false, true},
		{"pending", "in_progress", "success", sha, 15368, false, true},
		{"forged source", "completed", "success", sha, 99, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := check{Name: "Invariant Gate", HeadSHA: tt.sha, Status: tt.status, Conclusion: tt.conclusion}
			c.App.ID = tt.app
			checks := []check{c}
			if tt.missing {
				checks = nil
			}
			if err := verifyChecks([]string{"Invariant Gate"}, sha, checks); (err != nil) != tt.wantError {
				t.Fatalf("verifyChecks() = %v, want error %v", err, tt.wantError)
			}
		})
	}
	if err := verifyChecks(nil, sha, nil); err == nil {
		t.Fatal("empty mandatory checks passed")
	}
}

func TestProtectedChanges(t *testing.T) {
	t.Parallel()
	p := policy{Protected: []string{"invariants/", "tools/invariantcheck/", ".github/", "Makefile", "scripts/coverage-exceptions.txt"}}
	for _, tt := range []struct {
		name string
		file string
		want bool
	}{
		{"ordinary implementation", "internal/example/example.go", false},
		{"ordinary new test", "internal/example/example_test.go", false},
		{"remove definition", "invariants/README.md", true},
		{"rewrite own policy", "invariants/policy.json", true},
		{"delete gate", "tools/invariantcheck/main.go", true},
		{"replace workflow", ".github/workflows/invariants.yml", true},
		{"remove ownership", ".github/CODEOWNERS", true},
		{"lower threshold", "scripts/coverage-exceptions.txt", true},
		{"skip local gate", "Makefile", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := protect(p, []string{tt.file}); (err != nil) != tt.want {
				t.Fatalf("protect() = %v, want error %v", err, tt.want)
			}
		})
	}
}

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

func TestRunningEvidence(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	for _, tt := range []struct {
		name    string
		version string
		commit  string
		want    bool
	}{
		{"matching", "1.2.3", sha, false},
		{"old version", "1.2.2", sha, true},
		{"wrong build", "1.2.3", strings.Repeat("b", 40), true},
		{"ambiguous short commit", "1.2.3", sha[:7], true},
		{"missing commit", "1.2.3", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(map[string]map[string]string{"instance": {"version": tt.version, "commit": tt.commit}})
			if err != nil {
				t.Fatal(err)
			}
			if err := runningEvidence(data, "1.2.3", sha); (err != nil) != tt.want {
				t.Fatalf("runningEvidence() = %v, want error %v", err, tt.want)
			}
		})
	}
}

func TestCommandValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	name := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(name, []byte(`{"checks":["Invariant Gate"],"protected":["invariants/"],"tests":{"./example":["TestGuard"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-invalid"},
		{"-mode", "invalid"},
		{"-mode", "protect"},
		{"-mode", "release"},
		{"-mode", "running"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"-policy", name}, args...), &stdout, &stderr); code == 0 {
			t.Fatalf("run(%v) accepted invalid arguments", args)
		}
	}
	if _, err := readPolicy(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing policy accepted")
	}
}

func TestFullSHA(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		value string
		want  bool
	}{
		{strings.Repeat("a", 40), true},
		{strings.Repeat("0", 40), true},
		{strings.Repeat("z", 40), false},
		{"abc1234", false},
		{"--help", false},
	} {
		if got := fullSHA(tt.value); got != tt.want {
			t.Errorf("fullSHA(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}

func TestProtectedDeletionUsesTrustedPolicy(t *testing.T) {
	dir := t.TempDir()
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Example", "GIT_AUTHOR_EMAIL=example@example.com",
		"GIT_COMMITTER_NAME=Example", "GIT_COMMITTER_EMAIL=example@example.com")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "core.hooksPath=" + filepath.Join(dir, "no-hooks"), "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init")
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("rule.md", "Do not weaken tests.\n")
	write("policy.json", `{"checks":["Invariant Gate"],"protected":["rule.md","policy.json"],"tests":{"./example":["TestGuard"]}}`)
	git("add", ".")
	git("commit", "-m", "Initial approved policy")
	base := git("rev-parse", "HEAD")
	trusted := filepath.Join(t.TempDir(), "policy.json")
	data, err := os.ReadFile(filepath.Join(dir, "policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trusted, data, 0o600); err != nil {
		t.Fatal(err)
	}
	write("ordinary.go", "package example\n")
	git("add", ".")
	git("commit", "-m", "Ordinary implementation")
	ordinary := git("rev-parse", "HEAD")
	if err := os.Remove(filepath.Join(dir, "rule.md")); err != nil {
		t.Fatal(err)
	}
	write("policy.json", `{"checks":[],"protected":[],"tests":{}}`)
	git("add", "-A")
	git("commit", "-m", "Delete rule and exempt self")
	adversarial := git("rev-parse", "HEAD")
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tt := range []struct {
		name string
		head string
		want int
	}{
		{"ordinary change", ordinary, 0},
		{"self exemption and deletion", adversarial, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			got := run([]string{"-mode", "protect", "-policy", trusted, "-root", dir, "-base", base, "-head", tt.head}, &stdout, &stderr)
			if got != tt.want {
				t.Fatalf("run() = %d, want %d: %s", got, tt.want, &stderr)
			}
		})
	}
}
