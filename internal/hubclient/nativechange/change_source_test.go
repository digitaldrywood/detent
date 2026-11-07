package nativechange

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativeRecoveredSourceRequiresCurrentVersion(t *testing.T) {
	for _, test := range []struct {
		name      string
		change    string
		version   string
		attempt   string
		base      string
		policy    string
		wantError bool
	}{
		{name: "original version", change: "change", version: "original"},
		{name: "new operator version at same head", change: "change", version: "new", wantError: true},
		{name: "own publication acknowledgement lost", change: "change", version: "new", attempt: "attempt", base: "base", policy: "policy"},
		{name: "own publication wrong base", change: "change", version: "new", attempt: "attempt", base: "other", policy: "policy", wantError: true},
		{name: "own publication wrong policy", change: "change", version: "new", attempt: "attempt", base: "base", policy: "other", wantError: true},
		{name: "another Change", change: "other", version: "new", attempt: "attempt", base: "base", policy: "policy", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			execution := &nativeExecution{data: tracker.NativeRunData{RunID: "run", AttemptID: "attempt", PolicyID: "policy"}, recoveredSource: &tracker.NativeChangeReference{ChangeID: "change", VersionID: "original", HeadSHA: "head"}}
			detail := tracker.ChangeDetail{Change: tracker.ChangeRequest{ID: test.change, CurrentVersion: test.version}, Versions: []tracker.ChangeVersion{{ID: test.version, ChangeVersionInput: tracker.ChangeVersionInput{RunID: "run", AttemptID: test.attempt, BaseSHA: test.base, HeadSHA: "head", PolicyID: test.policy}}}}
			if err := execution.requireRecoveredSource(detail, tracker.AttemptDiffRequest{BaseSHA: "base", HeadSHA: "head"}); (err != nil) != test.wantError {
				t.Fatalf("recovered source publication authority: %v", err)
			}
		})
	}
}

func TestNativeChangeSourceRecoversOnlyVerifiedOwner(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                       string
		status                                     int
		foreign, wrongHead, lostLease, wrongDiff   bool
		missingCapture, wrongWorktree              bool
		wantError                                  bool
		transferred, staleTransfer, recovered      bool
		foreignChange, dirty, unpublished, unknown bool
		available, retained, activePublication     bool
		uncertain                                  bool
		currentCheckpoint, cleanCheckpoint         bool
	}{
		{name: "owner recaptures missing artifact", status: http.StatusInternalServerError},
		{name: "transferred version retains exact selection during recapture", status: http.StatusInternalServerError, transferred: true},
		{name: "stale transfer advances to current published source", transferred: true, staleTransfer: true, available: true},
		{name: "stale transfer with current unpushed checkpoint advances", transferred: true, staleTransfer: true, currentCheckpoint: true, available: true},
		{name: "stale transfer with current clean checkpoint advances", transferred: true, staleTransfer: true, currentCheckpoint: true, cleanCheckpoint: true, available: true},
		{name: "current dirty checkpoint refuses substitution", transferred: true, staleTransfer: true, currentCheckpoint: true, dirty: true, available: true, wantError: true},
		{name: "current uncertain checkpoint refuses substitution", transferred: true, staleTransfer: true, currentCheckpoint: true, uncertain: true, available: true, wantError: true},
		{name: "current checkpoint cannot accept corrupt current bundle", transferred: true, staleTransfer: true, currentCheckpoint: true, missingCapture: true, wantError: true},
		{name: "recovered source advances without transfer selection", staleTransfer: true, recovered: true, available: true},
		{name: "recovered transfer advances to current published source", transferred: true, staleTransfer: true, recovered: true, available: true},
		{name: "different Change transfer refuses substitution", transferred: true, staleTransfer: true, foreignChange: true, available: true, wantError: true},
		{name: "different Change refuses substitution", transferred: true, staleTransfer: true, recovered: true, foreignChange: true, available: true, wantError: true},
		{name: "unknown version refuses substitution", transferred: true, staleTransfer: true, unknown: true, available: true, wantError: true},
		{name: "uncertain checkpoint refuses substitution", transferred: true, staleTransfer: true, uncertain: true, available: true, wantError: true},
		{name: "dirty checkpoint refuses substitution", transferred: true, staleTransfer: true, dirty: true, available: true, wantError: true},
		{name: "unpublished checkpoint refuses substitution", transferred: true, staleTransfer: true, unpublished: true, available: true, wantError: true},
		{name: "retained unpublished source refuses substitution", transferred: true, staleTransfer: true, retained: true, available: true, wantError: true},
		{name: "active publication refuses substitution", transferred: true, staleTransfer: true, activePublication: true, available: true, wantError: true},
		{name: "stale selection cannot accept corrupt current bundle", transferred: true, staleTransfer: true, missingCapture: true, wantError: true},
		{name: "owner recaptures corrupt artifact"},
		{name: "foreign runner cannot recapture", foreign: true, wantError: true},
		{name: "wrong local checkpoint cannot recapture", wrongHead: true, wantError: true},
		{name: "changed immutable diff cannot recapture", wrongDiff: true, wantError: true},
		{name: "owner workspace cannot be read", missingCapture: true, wantError: true},
		{name: "local worktree differs from checkpoint", wrongWorktree: true, wantError: true},
		{name: "lease lost during capture", lostLease: true, wantError: true},
		{name: "authorization denial cannot recapture", status: http.StatusForbidden, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHub(t)
			issue := h.createInProgress(t, "Recover owner source")
			h.claim(t, issue.ID)
			execution, ok := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			if !ok {
				t.Fatal("native execution missing")
			}
			repo := nativeChangeSourceRepo(t)
			head := func() string {
				data, err := exec.CommandContext(t.Context(), "git", "-C", repo, "rev-parse", "HEAD").Output()
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(string(data))
			}
			base := head()
			if err := os.WriteFile(filepath.Join(repo, "CHANGE.md"), []byte("saved work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			nativeChangeGit(t, repo, "add", "CHANGE.md")
			nativeChangeGit(t, repo, "commit", "-m", "preserved source")
			exactHead := head()
			capture, err := workspace.CaptureChangeSource(t.Context(), repo, base, exactHead)
			if err != nil {
				t.Fatal(err)
			}
			item := tracker.NativeWorkItemID(issue.ID)
			change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: issue.Title})
			if err != nil {
				t.Fatal(err)
			}
			if test.wrongDiff {
				capture.Source.DiffSHA256 = strings.Repeat("d", 64)
			}
			version, err := h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{
				Mutation: nativeMutationKey(), ChangeVersionInput: tracker.ChangeVersionInput{
					BaseSHA: base, HeadSHA: exactHead, MergeBaseSHA: base, Repository: nativeChangeRepository,
					Code:      tracker.ChangeArtifact{Kind: "code", URI: nativeChangeRepository + "/commit/" + exactHead, SHA256: policy.Digest([]byte(exactHead)), Availability: "unverified"},
					Artifacts: []tracker.ChangeArtifact{}, PolicyID: h.descriptor.ID, Source: &capture.Source,
				}, SourceBundle: capture.Bundle,
			})
			if err != nil {
				t.Fatal(err)
			}
			owner := execution.claim.lease.MachineID
			if test.foreign {
				owner = "other"
			}
			checkpointHead := exactHead
			if test.wrongHead {
				checkpointHead = base
			}
			execution.claim.recovery.SourceAttemptID = "saved-source"
			execution.claim.recovery.Change = &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
			if test.transferred {
				execution.claim.recovery.SourceVersionID = version.ID
			}
			if test.recovered {
				execution.recoveredSource = &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: exactHead}
			}
			if test.activePublication {
				execution.change = &runner.NativeChange{ChangeID: change.ID, VersionID: version.ID, HeadSHA: exactHead}
			}
			if test.staleTransfer {
				if err := os.WriteFile(filepath.Join(repo, "NEWER.md"), []byte("newer published work\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				nativeChangeGit(t, repo, "add", "NEWER.md")
				nativeChangeGit(t, repo, "commit", "-m", "newer source")
				capture, err = workspace.CaptureChangeSource(t.Context(), repo, base, head())
				if err != nil {
					t.Fatal(err)
				}
				input := version.ChangeVersionInput
				input.HeadSHA, input.Source = head(), &capture.Source
				input.Code.SHA256 = strings.Repeat("a", 64)
				expected := version.ID
				if test.foreignChange {
					change, err = h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Other Change"})
					if err != nil {
						t.Fatal(err)
					}
					expected = ""
				}
				version, err = h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{
					Mutation: nativeMutationKey(), ExpectedVersionID: expected, ChangeVersionInput: input, SourceBundle: capture.Bundle,
				})
				if err != nil {
					t.Fatal(err)
				}
				if version.ID == execution.claim.recovery.SourceVersionID {
					t.Fatal("publication did not advance the current version")
				}
			}
			execution.claim.recovery.Attempts = []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "saved-source", MachineID: owner}, Checkpoint: &tracker.NativeCheckpoint{Resume: "resume_session", WorktreeState: "unpushed", HeadSHA: checkpointHead, Availability: "available", Storage: "local_only"}}}
			if test.unknown {
				execution.claim.recovery.SourceVersionID = "version_unknown"
			}
			checkpoint := execution.claim.recovery.SourceAttempt().Checkpoint
			if test.currentCheckpoint {
				checkpoint.HeadSHA = version.HeadSHA
			}
			if test.cleanCheckpoint {
				checkpoint.WorktreeState = "clean"
			}
			if test.uncertain {
				checkpoint.ExternalEffect, checkpoint.EffectState = "git_push", "ambiguous"
			}
			if test.dirty {
				checkpoint.WorktreeState, checkpoint.WorkspaceDigest = "dirty", "unpublished-digest"
			}
			if test.unpublished {
				checkpoint.HeadSHA = strings.Repeat("e", 40)
			}
			if test.retained {
				execution.retainedSource = &tracker.ChangeSourceCapture{Source: tracker.ChangeSource{HeadSHA: strings.Repeat("e", 40)}}
			}
			original := fixtureClient(h.native.Transport()).requests.Load()
			downloads := 0
			fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if strings.HasSuffix(request.URL.Path, "/source") {
					downloads++
					status := test.status
					body := []byte("corrupt source")
					if test.available {
						body = capture.Bundle
					}
					if status != 0 {
						body = []byte(`{"error":{"code":"internal_error","message":"source unavailable"}}`)
					} else {
						status = http.StatusOK
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
				}
				return original.RoundTrip(request)
			}))
			if test.wrongWorktree {
				nativeChangeGit(t, repo, "checkout", "--detach", base)
			}
			calls := 0
			if !test.missingCapture {
				execution.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
					calls++
					if test.lostLease {
						h.scheduler.ForgetClaim(issue.ID)
					}
					return workspace.CaptureChangeSource(ctx, repo, base, head)
				})
			}
			pending := filepath.Join(repo, "PENDING.md")
			if err := os.WriteFile(pending, []byte("pending local work"), 0o600); err != nil {
				t.Fatal(err)
			}
			recovery := execution.claim.recovery
			execution.recovery.Store(&recovery)
			selected := execution.claim.recovery.SourceVersionID
			previousSource, retainedSource := execution.recoveredSource, execution.retainedSource
			recovered, err := execution.RecoverChangeSource(t.Context())
			if (err != nil) != test.wantError {
				t.Fatalf("recovery error = %v", err)
			}
			if test.staleTransfer && test.wantError && !test.missingCapture {
				if !errors.Is(err, runner.ErrChangeSourceVersionSuperseded) {
					t.Fatalf("stale selection lost refusal classification: %v", err)
				}
				for _, identity := range []string{change.ID, selected, version.ID} {
					if !strings.Contains(err.Error(), identity) {
						t.Errorf("refusal omits source identity %q: %v", identity, err)
					}
				}
				if !strings.Contains(err.Error(), "re-read source recovery") || !strings.Contains(err.Error(), "before transfer") {
					t.Errorf("refusal omits reconciliation action: %v", err)
				}
				if calls != 0 || downloads != 0 || recovered.Version.ID != "" || execution.claim.recovery.SourceVersionID != selected || execution.recoveredSource != previousSource {
					t.Fatal("stale selection downloaded, recaptured or substituted source")
				}
				if err := execution.Validate(t.Context()); err != nil {
					t.Fatalf("source refusal changed execution authority: %v", err)
				}
			}
			wantCalls := 1
			if test.available {
				wantCalls = 0
			}
			if !test.wantError && (calls != wantCalls || recovered.Version.ID != version.ID || recovered.Version.BaseSHA != base || recovered.Version.HeadSHA != version.HeadSHA || !bytes.Equal(recovered.Bundle, capture.Bundle)) {
				t.Fatalf("exact source not recovered: %+v calls=%d", recovered.Version, calls)
			}
			if !test.wantError && recovered.Transferred != (test.transferred && !test.staleTransfer) {
				t.Fatal("source recovery lost transfer binding")
			}
			if test.wantError && (execution.recoveredSource != previousSource || execution.retainedSource != retainedSource) {
				t.Fatal("refusal changed recovered or retained unpublished source")
			}
			if !test.wantError && (execution.recoveredSource == nil || execution.recoveredSource.ChangeID != change.ID || execution.recoveredSource.VersionID != version.ID || execution.recoveredSource.HeadSHA != version.HeadSHA) {
				t.Fatal("recovery did not retain the validated current publication identity")
			}
			if test.currentCheckpoint && !test.wantError {
				retried, err := execution.RecoverChangeSource(t.Context())
				if err != nil || retried.Version.ID != version.ID || !bytes.Equal(retried.Bundle, capture.Bundle) {
					t.Fatalf("repeated recovery did not retain current published source: %q, %v", retried.Version.ID, err)
				}
			}
			if test.foreign || test.wrongHead || test.status == http.StatusForbidden {
				if calls != 0 {
					t.Fatal("denied recovery read local source")
				}
			}
			if test.lostLease && !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
				t.Fatalf("lost lease accepted: %v", err)
			}
			if data, err := os.ReadFile(pending); err != nil || string(data) != "pending local work" {
				t.Fatalf("pending work changed: %q %v", data, err)
			}
		})
	}
}
