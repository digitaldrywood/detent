package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
	cmd := newUpdateCommand(context.Background(), func(context.Context) (updateRunner, error) {
		return fake, nil
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--yes"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
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
	cmd := newUpdateCommand(context.Background(), func(context.Context) (updateRunner, error) {
		return fake, nil
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--from-release"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
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
		"Run the Go install for me",
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
	cmd := newUpdateCommand(context.Background(), func(context.Context) (updateRunner, error) {
		return fake, nil
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--json"})

	err := cmd.ExecuteContext(context.Background())
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
