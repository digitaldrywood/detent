package releaseprovenance

import (
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
	home, err := newGPGHome()
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
	run := func(name string, args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), name, args...)
		command.Dir = directory
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v: %s", name, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run("git", "init", "--quiet")
	run("git", "config", "maintenance.auto", "false")
	run("git", "config", "gc.auto", "0")
	run("git", "config", "user.name", "Example Operator")
	run("git", "config", "user.email", "operator@example.com")
	keys := make([]string, 0, 2)
	for _, email := range []string{"owner@example.com", "other@example.com"} {
		run("gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--quick-generate-key", email, "ed25519", "cert", "0")
		listing := run("gpg", "--batch", "--with-colons", "--list-keys", email)
		for line := range strings.SplitSeq(listing, "\n") {
			fields := strings.Split(line, ":")
			if fields[0] == "fpr" {
				keys = append(keys, fields[9])
				break
			}
		}
		run("gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--quick-add-key", keys[len(keys)-1], "ed25519", "sign", "0")
	}
	public := run("gpg", "--armor", "--export", keys[0], keys[1])
	ownerPublic := run("gpg", "--armor", "--export", keys[0])
	if err := os.Mkdir(filepath.Join(directory, ".github"), 0o700); err != nil {
		t.Fatal(err)
	}
	for index, test := range []struct {
		name, list                                                                        string
		signer                                                                            int
		gitAttack                                                                         string
		missing, wrongCommit, tamper, wrongReason, forged, uncommitted, blank, missingKey bool
		wantError                                                                         bool
		auditEnabled, noRequirements                                                      bool
	}{
		{name: "listed primary key authorizes signing subkey", signer: 0},
		{name: "signed override without mandatory checks", noRequirements: true},
		{name: "signed override with opted in audit", auditEnabled: true},
		{name: "unsigned annotation fails", signer: -1, wantError: true},
		{name: "unlisted key in committed keyring fails", signer: 1, wantError: true},
		{name: "replacement commit cannot authorize unlisted key", signer: 1, gitAttack: "replace", wantError: true},
		{name: "inherited object directory cannot authorize unlisted key", signer: 1, gitAttack: "objects", wantError: true},
		{name: "inherited alternates cannot authorize unlisted key", signer: 1, gitAttack: "alternates", wantError: true},
		{name: "mismatched commit content cannot authorize unlisted key", signer: 1, gitAttack: "content", wantError: true},
		{name: "mismatched root tree cannot authorize unlisted key", signer: 1, gitAttack: "root-tree", wantError: true},
		{name: "mismatched policy tree cannot authorize unlisted key", signer: 1, gitAttack: "policy-tree", wantError: true},
		{name: "mismatched signer blob cannot authorize unlisted key", signer: 1, gitAttack: "signer-blob", wantError: true},
		{name: "mismatched public key blob invalidates approval", gitAttack: "keyring-blob", wantError: true},
		{name: "inherited repository cannot redirect approval", gitAttack: "repository"},
		{name: "inherited config cannot affect approval", gitAttack: "config"},
		{name: "global config cannot affect approval", gitAttack: "global"},
		{name: "system config cannot affect approval", gitAttack: "system"},
		{name: "ambient keyring cannot supply a missing committed public key", signer: 1, list: keys[1], missingKey: true, wantError: true},
		{name: "missing approval fails", missing: true, wantError: true},
		{name: "different commit fails", wrongCommit: true, wantError: true},
		{name: "tampered signed reason fails", tamper: true, wantError: true},
		{name: "provenance reason must match approval", wrongReason: true, wantError: true},
		{name: "empty signer list fails", list: "\n", wantError: true},
		{name: "short key ID fails", list: keys[0][24:], wantError: true},
		{name: "forged environment cannot authorize unlisted key", signer: 1, forged: true, wantError: true},
		{name: "caller identity has no effect on valid approval", forged: true},
		{name: "working tree signer changes have no effect", signer: 1, uncommitted: true, wantError: true},
		{name: "blank reason fails", blank: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			list := test.list
			if list == "" {
				list = keys[0] + "\n"
			}
			committedPublic := public
			if test.missingKey {
				committedPublic = ownerPublic
			}
			for name, contents := range map[string]string{"release-override-signers": list, "release-override-signers.asc": committedPublic} {
				if err := os.WriteFile(filepath.Join(directory, ".github", name), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			run("git", "add", ".github")
			run("git", "-c", "commit.gpgsign=false", "commit", "--quiet", "--allow-empty", "-m", "example source")
			commit := run("git", "rev-parse", "HEAD")
			reason := "Owner accepts \"known\" failure --> investigate\nnext"
			manifest := Manifest{Schema: Schema, Repository: "example-org/example-project", Tag: fmt.Sprintf("v1.2.%d", index), Commit: commit,
				ValidationOverride: &ValidationOverride{Reason: reason, Status: "failure", StatusID: 9}}
			if test.auditEnabled {
				manifest.Checks = []Check{{Name: "release-security-audit", Status: "completed", Conclusion: "success", StatusID: 10}}
			}
			target := commit
			if test.wrongCommit {
				run("git", "-c", "commit.gpgsign=false", "commit", "--quiet", "--allow-empty", "-m", "different source")
				target = run("git", "rev-parse", "HEAD")
			}
			approvalTag := "release-override/" + manifest.Tag
			if !test.missing {
				message := reason
				if test.blank {
					message = " "
				}
				args := []string{"-c", "gpg.format=openpgp", "-c", "tag.gpgsign=false", "tag", "--cleanup=verbatim", "-a", approvalTag, target, "-m", message}
				if test.signer >= 0 {
					args[3] = "tag.gpgsign=true"
					args = append([]string{"-c", "user.signingkey=" + keys[test.signer]}, args...)
				}
				run("git", args...)
			}
			if test.tamper {
				object := run("git", "cat-file", "tag", "refs/tags/"+approvalTag)
				command := exec.CommandContext(t.Context(), "git", "hash-object", "-t", "tag", "-w", "--stdin")
				command.Dir = directory
				command.Stdin = strings.NewReader(strings.Replace(object, "Owner accepts", "Owner rejects", 1) + "\n")
				output, err := command.Output()
				if err != nil {
					t.Fatal(err)
				}
				run("git", "update-ref", "refs/tags/"+approvalTag, strings.TrimSpace(string(output)))
			}
			if test.wrongReason {
				manifest.ValidationOverride.Reason = "A forged reason"
			}
			if test.uncommitted {
				if err := os.WriteFile(filepath.Join(directory, ".github/release-override-signers"), []byte(keys[1]+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.forged {
				for name, value := range map[string]string{"DETENT_RELEASE_OVERRIDE": reason, "RELEASE_OVERRIDE_ACTORS": `["example-owner"]`, "GITHUB_ACTIONS": "true", "GITHUB_ACTOR": "example-owner", "GITHUB_RUN_ID": "12345", "WORKFLOW_OVERRIDE_ACTORS": `["example-owner"]`} {
					t.Setenv(name, value)
				}
			}
			switch test.gitAttack {
			case "root-tree", "policy-tree", "signer-blob", "keyring-blob":
				if _, err := VerifyOverrideTag(t.Context(), directory, manifest); (err != nil) != (test.signer == 1) {
					t.Fatalf("unexpected approval before loose object attack: %v", err)
				}
				if err := os.WriteFile(filepath.Join(directory, ".github/release-override-signers"), []byte(keys[1]+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if test.gitAttack == "keyring-blob" {
					if err := os.WriteFile(filepath.Join(directory, ".github/release-override-signers.asc"), []byte(ownerPublic), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				run("git", "add", ".github")
				run("git", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "attacker policy")
				path := map[string]string{
					"root-tree": ":", "policy-tree": ":.github",
					"signer-blob": ":.github/release-override-signers", "keyring-blob": ":.github/release-override-signers.asc",
				}[test.gitAttack]
				originalID := run("git", "rev-parse", commit+path)
				forgedID := run("git", "rev-parse", "HEAD"+path)
				objects := filepath.Join(directory, ".git", "objects")
				original := filepath.Join(objects, originalID[:2], originalID[2:])
				contents, err := os.ReadFile(original)
				if err != nil {
					t.Fatal(err)
				}
				forged, err := os.ReadFile(filepath.Join(objects, forgedID[:2], forgedID[2:]))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(original, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.WriteFile(original, contents, 0o600); err != nil {
						t.Error(err)
					}
				})
				if err := os.WriteFile(original, forged, 0o600); err != nil {
					t.Fatal(err)
				}
			case "replace", "objects", "alternates", "content":
				if _, err := VerifyOverrideTag(t.Context(), directory, manifest); err == nil {
					t.Fatal("unlisted key authorized before Git object attack")
				}
				if err := os.WriteFile(filepath.Join(directory, ".github/release-override-signers"), []byte(keys[1]+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				run("git", "add", ".github")
				run("git", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "attacker signer list")
				attacker := run("git", "rev-parse", "HEAD")
				if test.gitAttack == "replace" {
					run("git", "replace", commit, attacker)
				} else {
					objects := filepath.Join(directory, ".git", "objects")
					alternate := t.TempDir()
					if err := os.CopyFS(alternate, os.DirFS(objects)); err != nil {
						t.Fatal(err)
					}
					forged, err := os.ReadFile(filepath.Join(objects, attacker[:2], attacker[2:]))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(alternate, commit[:2], commit[2:]), forged, 0o600); err != nil {
						t.Fatal(err)
					}
					switch test.gitAttack {
					case "objects":
						t.Setenv("GIT_OBJECT_DIRECTORY", alternate)
					case "content":
						original := filepath.Join(objects, commit[:2], commit[2:])
						contents, err := os.ReadFile(original)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.Chmod(original, 0o600); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := os.WriteFile(original, contents, 0o600); err != nil {
								t.Error(err)
							}
						})
						if err := os.WriteFile(original, forged, 0o600); err != nil {
							t.Fatal(err)
						}
					case "alternates":
						original := filepath.Join(objects, commit[:2], commit[2:])
						backup := filepath.Join(t.TempDir(), "commit")
						if err := os.Rename(original, backup); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := os.Rename(backup, original); err != nil {
								t.Error(err)
							}
						})
						t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", alternate)
					}
				}
			case "repository":
				foreign := t.TempDir()
				run("git", "init", "--bare", foreign)
				t.Setenv("GIT_DIR", foreign)
				t.Setenv("GIT_COMMON_DIR", foreign)
			case "config", "global", "system":
				config := filepath.Join(t.TempDir(), "config")
				if err := os.WriteFile(config, []byte("[invalid\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				switch test.gitAttack {
				case "config":
					t.Setenv("GIT_CONFIG_PARAMETERS", "invalid")
				case "global":
					t.Setenv("GIT_CONFIG_GLOBAL", config)
				case "system":
					t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
					t.Setenv("GIT_CONFIG_SYSTEM", config)
				}
			}
			signature, signatureErr := VerifyOverrideTag(t.Context(), directory, manifest)
			if (signatureErr != nil) != test.wantError {
				var exitError *exec.ExitError
				if errors.As(signatureErr, &exitError) {
					t.Logf("fixture stderr: %s", exitError.Stderr)
				}
				t.Fatalf("VerifyOverrideTag() = %v, want error %t", signatureErr, test.wantError)
			}
			evidence := GitHubEvidence{RequiredChecks: []RepositoryRequirement{{Name: "scheduled-full-ci"}},
				Checks: []ObservedCheck{{Name: "scheduled-full-ci", Commit: commit, Status: "completed", Conclusion: "failure", StatusID: 9}}, OverrideSignature: signature}
			if test.auditEnabled {
				evidence.RequiredChecks = append(evidence.RequiredChecks, RepositoryRequirement{Name: "release-security-audit"})
				evidence.Checks = append(evidence.Checks, ObservedCheck{Name: "release-security-audit", Commit: commit, Status: "completed", Conclusion: "success", StatusID: 10})
			}
			if test.noRequirements {
				evidence.RequiredChecks = nil
				evidence.Checks = nil
			}
			if err := VerifyGitHubEvidence(manifest, evidence); (err != nil) != test.wantError {
				t.Fatalf("VerifyGitHubEvidence() = %v, want error %t", err, test.wantError)
			}
		})
	}
}
