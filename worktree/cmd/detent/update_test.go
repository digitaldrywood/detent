package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/update"
)

func TestUpdateCommandCheckJSON(t *testing.T) {
	t.Parallel()

	fake := &fakeUpdater{
		checkStatus: update.Status{
			CurrentVersion:  "1.2.3",
			LatestVersion:   "1.2.4",
			LatestTag:       "v1.2.4",
			UpdateAvailable: true,
			InstallSource:   update.InstallSourceRelease,
			Action:          update.ActionAvailable,
			Message:         "Detent 1.2.3 can be updated to 1.2.4.",
		},
	}
	cmd := newUpdateCommand(context.Background(), func(context.Context) (updateRunner, error) {
		return fake, nil
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--check", "--json"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if !fake.checkCalled {
		t.Fatal("Check() was not called")
	}
	if fake.applyCalled {
		t.Fatal("Apply() was called for --check")
	}

	var got update.Status
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v\n%s", err, stdout.String())
	}
	if !got.UpdateAvailable {
		t.Fatal("UpdateAvailable = false, want true")
	}
	if got.LatestVersion != "1.2.4" {
		t.Fatalf("LatestVersion = %q, want 1.2.4", got.LatestVersion)
	}
}

func TestUpdateCommandYesAppliesWithoutPrompt(t *testing.T) {
	t.Setenv("DETENT_FORMAT", "pretty")

	fake := &fakeUpdater{
		applyStatus: update.Status{
			CurrentVersion:  "1.2.3",
			LatestVersion:   "1.2.4",
			UpdateAvailable: true,
			InstallSource:   update.InstallSourceRelease,
			Action:          update.ActionUpdated,
			Message:         "Updated Detent from 1.2.3 to 1.2.4.",
		},
	}
	root, cmd := updateCommandWithConfig(t, filepath.Join(t.TempDir(), "global.yaml"), func(context.Context) (updateRunner, error) {
		return fake, nil
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"update", "--yes"})

	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if !fake.applyCalled {
		t.Fatal("Apply() was not called")
	}
	if !fake.applyOptions.AssumeYes {
		t.Fatal("AssumeYes = false, want true")
	}
	if fake.applyOptions.Confirm != nil {
		t.Fatal("Confirm is set for --yes")
	}
	if fake.applyOptions.SelectGoInstallAction != nil {
		t.Fatal("SelectGoInstallAction is set for --yes")
	}
	if !strings.Contains(stdout.String(), "Updated Detent from 1.2.3 to 1.2.4.") {
		t.Fatalf("stdout = %q, want update message", stdout.String())
	}
}

func TestUpdateCommandDefaultsToJSONWhenStdoutIsNotTTY(t *testing.T) {
	t.Parallel()

	fake := &fakeUpdater{
		checkStatus: update.Status{
			CurrentVersion:  "1.2.3",
			LatestVersion:   "1.2.4",
			LatestTag:       "v1.2.4",
			UpdateAvailable: true,
			InstallSource:   update.InstallSourceRelease,
			Action:          update.ActionAvailable,
			Message:         "Detent 1.2.3 can be updated to 1.2.4.",
		},
	}
	cmd := newUpdateCommand(context.Background(), func(context.Context) (updateRunner, error) {
		return fake, nil
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--check"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}

	var got update.Status
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v\n%s", err, stdout.String())
	}
	if !got.UpdateAvailable {
		t.Fatal("UpdateAvailable = false, want true")
	}
}

func TestUpdateCommandFromReleasePassesOptionWithoutPrompt(t *testing.T) {
	t.Parallel()

	fake := &fakeUpdater{
		applyStatus: update.Status{
			CurrentVersion:  "1.2.3",
			LatestVersion:   "1.2.4",
			UpdateAvailable: true,
			InstallSource:   update.InstallSourceGoInstall,
			Action:          update.ActionUpdated,
			Message:         "Updated Detent from 1.2.3 to 1.2.4.",
		},
	}
	root, cmd := updateCommandWithConfig(t, filepath.Join(t.TempDir(), "global.yaml"), func(context.Context) (updateRunner, error) {
		return fake, nil
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"update", "--from-release"})

	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if !fake.applyCalled {
		t.Fatal("Apply() was not called")
	}
	if !fake.applyOptions.FromRelease {
		t.Fatal("FromRelease = false, want true")
	}
	if fake.applyOptions.Confirm != nil {
		t.Fatal("Confirm is set for --from-release")
	}
	if fake.applyOptions.SelectGoInstallAction != nil {
		t.Fatal("SelectGoInstallAction is set for --from-release")
	}
}

func TestDefaultUpdateConfigUsesResolvedVersionInfo(t *testing.T) {
	t.Parallel()

	const commit = "0123456789abcdef0123456789abcdef01234567"
	cfg := defaultUpdateConfig("/tmp/detent", versionInfo{Version: "v0.4.0", Commit: commit}, "linux", "amd64")

	if cfg.CurrentVersion != "v0.4.0" {
		t.Fatalf("CurrentVersion = %q, want v0.4.0", cfg.CurrentVersion)
	}
	if cfg.CurrentCommit != commit {
		t.Fatalf("CurrentCommit = %q, want %q", cfg.CurrentCommit, commit)
	}
	if cfg.ExecutablePath != "/tmp/detent" {
		t.Fatalf("ExecutablePath = %q, want /tmp/detent", cfg.ExecutablePath)
	}
	if cfg.GOOS != "linux" {
		t.Fatalf("GOOS = %q, want linux", cfg.GOOS)
	}
	if cfg.GOARCH != "amd64" {
		t.Fatalf("GOARCH = %q, want amd64", cfg.GOARCH)
	}
}

func TestUpdateGitHubTokenPrefersDetentToken(t *testing.T) {
	t.Setenv("DETENT_GITHUB_TOKEN", "detent-token")
	t.Setenv("GITHUB_TOKEN", "github-token")

	if got := updateGitHubToken(); got != "detent-token" {
		t.Fatalf("updateGitHubToken() = %q, want detent-token", got)
	}
}

func TestUpdateGitHubTokenIgnoresGenericGitHubToken(t *testing.T) {
	t.Setenv("DETENT_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "github-token")

	if got := updateGitHubToken(); got != "" {
		t.Fatalf("updateGitHubToken() = %q, want empty", got)
	}
}

func TestSelectGoInstallActionParsesReleaseChoice(t *testing.T) {
	t.Parallel()

	cmd := newUpdateCommand(context.Background(), func(context.Context) (updateRunner, error) {
		return &fakeUpdater{}, nil
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetIn(strings.NewReader("2\n"))

	choice, err := selectGoInstallAction(cmd)(update.Status{
		CurrentVersion: "1.2.3",
		LatestVersion:  "1.2.4",
		Binary:         "/tmp/detent",
		Command:        "go install github.com/digitaldrywood/detent/cmd/detent@latest",
	})
	if err != nil {
		t.Fatalf("selectGoInstallAction() error = %v", err)
	}
	if choice != update.GoInstallActionRelease {
		t.Fatalf("choice = %q, want %q", choice, update.GoInstallActionRelease)
	}

	output := stdout.String()
	for _, want := range []string{
		"Build the prepared source archive manually",
		"Switch to the release binary",
		"Abort",
		"WARNING:",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("prompt missing %q:\n%s", want, output)
		}
	}
}

func TestUpdateCommandJSONWritesRefusalStatus(t *testing.T) {
	t.Parallel()

	fake := &fakeUpdater{
		applyStatus: update.Status{
			CurrentVersion: "dev",
			InstallSource:  update.InstallSourceDevelopment,
			Action:         update.ActionRefused,
			Message:        "This Detent binary does not include release version metadata.",
		},
		applyErr: update.ErrRefused,
	}
	root, cmd := updateCommandWithConfig(t, filepath.Join(t.TempDir(), "global.yaml"), func(context.Context) (updateRunner, error) {
		return fake, nil
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"update", "--json"})

	err := root.ExecuteContext(t.Context())
	if !errors.Is(err, update.ErrRefused) {
		t.Fatalf("ExecuteContext() error = %v, want %v", err, update.ErrRefused)
	}
	if fake.applyOptions.Confirm != nil {
		t.Fatal("Confirm is set for --json")
	}

	var got update.Status
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v\n%s", err, stdout.String())
	}
	if got.Action != update.ActionRefused {
		t.Fatalf("Action = %q, want %q", got.Action, update.ActionRefused)
	}
}

type fakeUpdater struct {
	checkCalled  bool
	applyCalled  bool
	applyOptions update.ApplyOptions
	checkStatus  update.Status
	checkErr     error
	applyStatus  update.Status
	applyErr     error
}

func (f *fakeUpdater) Check(context.Context) (update.Status, error) {
	f.checkCalled = true
	return f.checkStatus, f.checkErr
}

func (f *fakeUpdater) Apply(_ context.Context, opts update.ApplyOptions) (update.Status, error) {
	f.applyCalled = true
	f.applyOptions = opts
	return f.applyStatus, f.applyErr
}

func TestUpdateCommandPreservesVersionOnError(t *testing.T) {
	for _, tt := range []struct{ name, version string }{{"empty coordination status", ""}, {"server version", "v0.114.15"}} {
		t.Run(tt.name, func(t *testing.T) {
			failure := errors.New("dashboard API is unreachable")
			root, cmd := updateCommandWithConfig(t, filepath.Join(t.TempDir(), "global.yaml"), func(context.Context) (updateRunner, error) {
				return &fakeUpdater{applyStatus: update.Status{CurrentVersion: tt.version}, applyErr: failure}, nil
			})
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&bytes.Buffer{})
			root.SetArgs([]string{"update", "--yes", "--json"})
			if err := root.ExecuteContext(t.Context()); !errors.Is(err, failure) {
				t.Fatalf("error=%v", err)
			}
			var status update.Status
			if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			want := tt.version
			if want == "" {
				want = currentVersionInfo().Version
			}
			if status.CurrentVersion == "" || status.CurrentVersion != want {
				t.Fatalf("version=%q want %q", status.CurrentVersion, want)
			}
		})
	}
}

func TestUpdateSafetyRefusesRunningInstance(t *testing.T) {
	for _, state := range []string{"absent", "held", "released"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			configPath := filepath.Join(t.TempDir(), "global.yaml")
			if state != "absent" {
				lock, err := instancelock.Acquire(filepath.Join(filepath.Dir(configPath), "detent.db.lock"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := lock.Close(); err != nil {
						t.Error(err)
					}
				})
				if state == "released" {
					if err := lock.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			fake := &fakeUpdater{}
			root, _ := updateCommandWithConfig(t, configPath, func(context.Context) (updateRunner, error) { return fake, nil })
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs([]string{"update", "--yes"})
			err := root.ExecuteContext(t.Context())
			if state == "held" {
				if err == nil || !strings.Contains(err.Error(), "stop the running Detent instance") {
					t.Fatalf("running update error = %v", err)
				}
				if fake.applyCalled {
					t.Fatal("offline apply was called with a running instance")
				}
				return
			}
			if err != nil || !fake.applyCalled {
				t.Fatalf("offline update applied=%t, error=%v", fake.applyCalled, err)
			}
			if fake.applyOptions.Preflight == nil || fake.applyOptions.RecoveryStatePath != update.RecoveryStatePath(configPath) {
				t.Fatalf("offline safety options = %+v", fake.applyOptions)
			}
		})
	}
}

func updateCommandWithConfig(t *testing.T, configPath string, factory func(context.Context) (updateRunner, error)) (*cobra.Command, *cobra.Command) {
	t.Helper()
	root := &cobra.Command{Use: "detent"}
	root.PersistentFlags().String("config", configPath, "")
	cmd := newUpdateCommand(t.Context(), factory)
	root.AddCommand(cmd)
	return root, cmd
}
