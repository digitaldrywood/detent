package releaseprovenance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		if err := os.Unsetenv(key); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

func TestOverrideTagRequiresAllowlistedSignature(t *testing.T) {
	directory := t.TempDir()
	home, err := os.MkdirTemp("", "gpg-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		command := exec.Command("gpgconf", "--homedir", home, "--kill", "gpg-agent")
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("stop fixture signing agent: %v: %s", err, output)
		}
		if err := os.RemoveAll(home); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("HOME", directory)
	t.Setenv("GNUPGHOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(t *testing.T, name string, args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), name, args...)
		command.Dir = directory
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v: %s", name, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run(t, "git", "init", "--quiet")
	run(t, "git", "config", "user.name", "Example Operator")
	run(t, "git", "config", "user.email", "operator@example.com")
	run(t, "git", "-c", "commit.gpgsign=false", "commit", "--quiet", "--allow-empty", "-m", "example source")
	commit := run(t, "git", "rev-parse", "HEAD")
	keys := make([]string, 0, 2)
	for _, email := range []string{"owner@example.com", "other@example.com"} {
		run(t, "gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--quick-generate-key", email, "ed25519", "sign", "0")
		listing := run(t, "gpg", "--batch", "--with-colons", "--list-keys", email)
		for line := range strings.SplitSeq(listing, "\n") {
			fields := strings.Split(line, ":")
			if fields[0] == "fpr" {
				keys = append(keys, fields[9])
				break
			}
		}
	}
	if len(keys) != 2 {
		t.Fatalf("fixture signing keys = %d, want 2", len(keys))
	}
	public := run(t, "gpg", "--armor", "--export", keys[0])
	keyJSON, err := json.Marshal([][]map[string]string{{{"key_id": keys[0][len(keys[0])-16:], "raw_key": public}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"keys.json": string(keyJSON),
		"gh": `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *'/actions/variables/RELEASE_OVERRIDE_ACTORS'*) cat "$HOME/actors.json" ;;
  *'users/example-operator/gpg_keys?'*) cat "$HOME/keys.json" ;;
  'api users/example-operator') printf '{"login":"example-operator","type":"User"}' ;;
  *) exit 1 ;;
esac
`,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	for index, test := range []struct {
		name, actors, workflowActors string
		signer                       int
		tamper, revoked              bool
		wantError                    bool
	}{
		{name: "unsigned annotation cannot waive failed suite", signer: -1, wantError: true},
		{name: "signature from another key cannot waive failed suite", signer: 1, wantError: true},
		{name: "allowlisted owner signature waives only failed suite", signer: 0},
		{name: "changed signed reason invalidates signature", signer: 0, tamper: true, wantError: true},
		{name: "empty allowlist rejects valid signature", signer: 0, actors: `[]`, wantError: true},
		{name: "different owner allowlist rejects valid signature", signer: 0, actors: `["other-user"]`, wantError: true},
		{name: "malformed allowlist rejects valid signature", signer: 0, actors: `{`, wantError: true},
		{name: "explicit workflow allowlist is authoritative", signer: 0, actors: `{`, workflowActors: `["example-operator"]`},
		{name: "revoked owner key rejects valid signature", signer: 0, revoked: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := Manifest{Schema: Schema, Repository: "example-org/example-project", Tag: fmt.Sprintf("v1.2.%d", index), Commit: commit,
				Checks:             []Check{{Name: "release-security-audit", Status: "completed", Conclusion: "success", StatusID: 10}},
				ValidationOverride: &ValidationOverride{Reason: "Owner accepts failed validation", Status: "failure", StatusID: 9}}
			annotation, err := Annotation(manifest)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"-c", "gpg.format=openpgp", "-c", "tag.gpgsign=false", "tag", "-a", manifest.Tag, commit, "-m", annotation}
			if test.signer >= 0 {
				args[3] = "tag.gpgsign=true"
				args = append([]string{"-c", "user.signingkey=" + keys[test.signer]}, args...)
			}
			run(t, "git", args...)
			if test.tamper {
				object := run(t, "git", "cat-file", "tag", "refs/tags/"+manifest.Tag)
				object = strings.Replace(object, "Owner accepts", "Owner rejects", 1)
				command := exec.CommandContext(t.Context(), "git", "hash-object", "-t", "tag", "-w", "--stdin")
				command.Dir = directory
				command.Stdin = strings.NewReader(object + "\n")
				output, err := command.Output()
				if err != nil {
					t.Fatal(err)
				}
				run(t, "git", "update-ref", "refs/tags/"+manifest.Tag, strings.TrimSpace(string(output)))
				manifest.ValidationOverride.Reason = "Owner rejects failed validation"
			}
			actors := test.actors
			if actors == "" {
				actors = `["example-operator"]`
			}
			if err := os.WriteFile(filepath.Join(directory, "actors.json"), []byte(actors), 0o600); err != nil {
				t.Fatal(err)
			}
			keysJSON := string(keyJSON)
			if test.revoked {
				keysJSON = strings.Replace(keysJSON, `"raw_key":`, `"revoked":true,"raw_key":`, 1)
			}
			if err := os.WriteFile(filepath.Join(directory, "keys.json"), []byte(keysJSON), 0o600); err != nil {
				t.Fatal(err)
			}
			signature, signatureErr := VerifyOverrideTag(t.Context(), directory, manifest, test.workflowActors)
			if (signatureErr != nil) != test.wantError {
				var exitError *exec.ExitError
				if errors.As(signatureErr, &exitError) {
					t.Logf("fixture command stderr: %s", exitError.Stderr)
				}
				t.Fatalf("VerifyOverrideTag() = %v, want error %t", signatureErr, test.wantError)
			}
			evidence := GitHubEvidence{RequiredChecks: []RepositoryRequirement{{Name: "scheduled-full-ci"}, {Name: "release-security-audit"}},
				Checks: []ObservedCheck{{Name: "scheduled-full-ci", Commit: commit, Status: "completed", Conclusion: "failure", StatusID: 9},
					{Name: "release-security-audit", Commit: commit, Status: "completed", Conclusion: "success", StatusID: 10}}, OverrideSignature: signature}
			if err := VerifyGitHubEvidence(manifest, evidence); (err != nil) != test.wantError {
				t.Fatalf("VerifyGitHubEvidence() = %v, want error %t", err, test.wantError)
			}
			if !test.wantError {
				manifest.ValidationOverride.Reason = "Different reason"
				if err := VerifyGitHubEvidence(manifest, evidence); err == nil {
					t.Fatal("verified signature authorized a different override reason")
				}
			}
		})
	}
}
