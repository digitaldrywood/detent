package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspace"
)

type landingBackend struct {
	workspace.Backend
	result       workspace.LandResult
	err          error
	received     workspace.LandOptions
	githubCalled bool
}

func (b *landingBackend) LandChange(_ context.Context, _ workspace.Info, _ workspace.Issue, opts workspace.LandOptions) (workspace.LandResult, error) {
	b.received = opts
	return b.result, b.err
}

func (b *landingBackend) LandChangeViaGitHub(_ context.Context, _ workspace.Info, _ workspace.Issue, opts workspace.LandOptions) (workspace.LandResult, error) {
	b.githubCalled = true
	b.received = opts
	return b.result, b.err
}

type landingStub struct {
	target    NativeLandingTarget
	targetErr error
	recordErr error
	recorded  []NativeLanding
}

func (s *landingStub) LandingTarget(context.Context) (NativeLandingTarget, error) {
	return s.target, s.targetErr
}

func (s *landingStub) RecordLanding(_ context.Context, landing NativeLanding) error {
	s.recorded = append(s.recorded, landing)
	return s.recordErr
}

func TestLandNativeChange(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("c", 40)
	merge := strings.Repeat("e", 40)
	target := NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Method: "merge", Title: "Add a sign-in link", Number: 2}
	for _, test := range []struct {
		name         string
		stub         landingStub
		backend      landingBackend
		wantOutput   string
		wantErr      string
		wantRecorded int
		wantRefusal  string
		wantGitHub   bool
	}{
		{name: "lands and records", stub: landingStub{target: target}, backend: landingBackend{result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "merge"}},
			wantOutput: RunOutputNativeLanded, wantRecorded: 1},
		{name: "opted-in project uses GitHub PR landing", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}, backend: landingBackend{result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "merge"}},
			wantOutput: RunOutputNativeLanded, wantRecorded: 1, wantGitHub: true},
		{name: "a refusal is reported, not recorded", stub: landingStub{target: target}, backend: landingBackend{err: &workspace.LandRefusal{Kind: workspace.LandRefusalProtected, Reason: "the base branch main refused the push"}},
			wantOutput: RunOutputNativeLandingRefused, wantRefusal: workspace.LandRefusalProtected},
		{name: "a GitHub conflict retains reviewed identity without a landing receipt", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}, backend: landingBackend{err: &workspace.LandRefusal{Kind: workspace.LandRefusalConflict, Reason: "Pull Request is not mergeable (HTTP 405)"}},
			wantOutput: RunOutputNativeLandingRefused, wantRefusal: workspace.LandRefusalConflict, wantGitHub: true},
		{name: "an unreviewed change is a refusal", stub: landingStub{target: target, targetErr: errors.New("hub says: " + ErrLandingNotReviewed.Error())},
			wantErr: "resolve landing target"},
		{name: "an unreviewed change from the hub is a refusal", stub: landingStub{target: target, targetErr: ErrLandingNotReviewed},
			wantOutput: RunOutputNativeLandingRefused, wantRefusal: workspace.LandRefusalNothing},
		{name: "a git failure fails the run", stub: landingStub{target: target}, backend: landingBackend{err: errors.New("git fetch origin: network down")},
			wantErr: "network down"},
		{name: "an unrecorded landing fails the run after retrying the report", stub: landingStub{target: target, recordErr: errors.New("hub unavailable")}, backend: landingBackend{result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "merge"}},
			wantErr: "record landing", wantRecorded: landingReportTries},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r := &Runner{}
			stub, backend := test.stub, test.backend
			result, err := r.landNativeChange(t.Context(), RunRequest{}, &stub, &backend, workspace.Info{Path: t.TempDir(), Branch: "detent/land"}, workspace.Issue{Identifier: "DD-1"})
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				if len(stub.recorded) != test.wantRecorded {
					t.Fatalf("recorded = %#v", stub.recorded)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.FinalState != FinalStateCompleted || result.Output != test.wantOutput || result.NativeLanding == nil {
				t.Fatalf("result = %#v", result)
			}
			if result.NativeLanding.RefusalKind != test.wantRefusal || result.NativeLanding.Landed != (test.wantRefusal == "") {
				t.Fatalf("landing = %#v", result.NativeLanding)
			}
			if result.NativeLanding.ChangeID != target.ChangeID || result.NativeLanding.VersionID != target.VersionID || result.NativeLanding.HeadSHA != head || test.wantRefusal != "" && result.NativeLanding.MergeSHA != "" {
				t.Fatalf("landing lost exact reviewed identity or invented a merge: %#v", result.NativeLanding)
			}
			if len(stub.recorded) != test.wantRecorded {
				t.Fatalf("recorded = %#v, want %d", stub.recorded, test.wantRecorded)
			}
			if backend.githubCalled != test.wantGitHub {
				t.Fatalf("GitHub landing called = %t, want %t", backend.githubCalled, test.wantGitHub)
			}
			if test.wantRecorded == 1 {
				if backend.received.HeadSHA != head || backend.received.Method != "merge" || !backend.received.PushAttemptBranch || (!test.wantGitHub && (!strings.Contains(backend.received.Message, "Add a sign-in link") || !strings.Contains(backend.received.Message, "round 2"))) {
					t.Fatalf("land options = %#v", backend.received)
				}
				if stub.recorded[0].MergeSHA != merge || stub.recorded[0].BaseRef != "main" || stub.recorded[0].ChangeID != "change_1" {
					t.Fatalf("recorded landing = %#v", stub.recorded[0])
				}
			}
		})
	}
}

func TestLandNativeChangeKeepsAnUnreportedLanding(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("c", 40)
	merge := strings.Repeat("e", 40)
	target := NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Method: "squash"}
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@example.test"}, {"config", "user.name", "t"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := gitCommand(dir, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	info := workspace.Info{Path: dir, Branch: "detent/land"}
	stub := landingStub{target: target, recordErr: errors.New("hub unavailable")}
	backend := landingBackend{result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "squash"}}
	r := &Runner{}
	_, err := r.landNativeChange(t.Context(), RunRequest{}, &stub, &backend, info, workspace.Issue{Identifier: "DD-1"})
	if err == nil || !strings.Contains(err.Error(), "record landing") {
		t.Fatalf("error = %v, want the report failure", err)
	}
	if len(stub.recorded) != landingReportTries {
		t.Fatalf("report attempts = %d, want %d", len(stub.recorded), landingReportTries)
	}
	kept, err := os.ReadFile(filepath.Join(dir, ".git", "detent-landing.json"))
	if err != nil {
		t.Fatalf("the pushed landing was not kept: %v", err)
	}
	if !strings.Contains(string(kept), merge) || !strings.Contains(string(kept), head) {
		t.Fatalf("kept landing = %s", kept)
	}
	// The next run reports it and forgets it.
	stub.recordErr = nil
	if _, err := r.landNativeChange(t.Context(), RunRequest{}, &stub, &backend, info, workspace.Issue{Identifier: "DD-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "detent-landing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the reported landing was kept: %v", err)
	}
}

func gitCommand(dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
