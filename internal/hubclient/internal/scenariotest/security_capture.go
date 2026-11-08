package scenariotest

import (
	"github.com/digitaldrywood/detent/internal/hubclient/changesource"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/validation"

	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func RunNativeSecurityAuditCapturesBeyondStoredDiff(t *testing.T) {
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	t.Parallel()
	for _, size := range []int{0, 300 << 10, 21 << 20, tracker.MaxChangeDiffBytes} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			directory := nativeChangeSourceRepo(t)
			gitOutput := func(args ...string) string {
				data, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory}, args...)...).Output()
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			base := strings.TrimSpace(gitOutput("rev-parse", "HEAD"))
			for path, content := range map[string]string{"large.txt": strings.Repeat("source line\n", size/11+1), "zz-late.txt": "late security change\n"} {
				if size == 0 {
					continue
				}
				if err := os.WriteFile(filepath.Join(directory, path), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			nativeChangeGit(t, directory, "add", ".")
			nativeChangeGit(t, directory, "commit", "--allow-empty", "-m", "audit source")
			head := strings.TrimSpace(gitOutput("rev-parse", "HEAD"))
			var original string
			var expected [sha256.Size]byte
			if size < tracker.MaxChangeDiffBytes {
				original = gitOutput("diff", "--no-ext-diff", "--no-textconv", "--find-renames", base, head, "--")
				if size > 0 && len(original) <= size {
					t.Fatal("fixture did not cross source boundary")
				}
				expected = sha256.Sum256([]byte(original))
			}
			captured, err := workspace.GitFileDiffs(t.Context(), directory, base, tracker.MaxDiffBytes)
			if err != nil {
				t.Fatal(err)
			}
			request := tracker.AttemptDiffRequest{BaseSHA: base, HeadSHA: head}
			for _, file := range captured.Files {
				request.Files = append(request.Files, tracker.AttemptDiffFile{Path: file.Path, Status: file.Status, Patch: file.Patch, Additions: file.Additions})
			}
			if captured.Truncated {
				request.Files = tracker.StripDiffPatches(request.Files)
			}
			request.Files, _ = tracker.NormalizeDiffFiles(request.Files)
			version := tracker.ChangeVersion{ID: "version_example", ChangeVersionInput: tracker.ChangeVersionInput{Repository: "https://github.com/example-org/example-project", BaseSHA: base, HeadSHA: head, PolicyID: "policy_example"}, Policy: policy.Descriptor{Gates: policy.Gates{SecurityAudit: true}}}
			var source tracker.ChangeSourceCapture
			if size != 0 {
				source, err = workspace.CaptureChangeSource(t.Context(), directory, base, head)
				if err != nil {
					t.Fatal(err)
				}
				version.Source = &source.Source
			}
			detail := tracker.ChangeDetail{Change: tracker.ChangeRequest{ID: "change_example", ProjectID: "prj_example", WorkItemID: "wi_example", CurrentVersion: version.ID}, Versions: []tracker.ChangeVersion{version}}
			runtime := tracker.NativeRuntimeEvidence{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_example"}}, Attempt: &tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{AttemptID: "attempt_example", PolicyID: version.PolicyID}}}
			client, err := createTestClient(t, Config{URL: "http://native-hub.test", TokenSource: func() string { return "token" }, HTTPClient: testsupport.HandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var value any
				switch {
				case strings.HasSuffix(r.URL.Path, "/capabilities"):
					value = struct {
						Features []string `json:"features"`
					}{Features: []string{tracker.NativeRuntimeEvidenceCapability}}
				case strings.HasSuffix(r.URL.Path, "/runtime"):
					value = runtime
				case strings.HasSuffix(r.URL.Path, "/changes/change_example"):
					page := operatortool.ChangeResult{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_example", Detail: &detail, Read: &operatortool.ChangeRead{VersionID: version.ID, Sections: map[string]operatortool.ChangeReadPage{}}}
					for _, section := range []string{"versions", "reviews", "checks", "discussion"} {
						page.Read.Sections[section] = operatortool.ChangeReadPage{Complete: true}
					}
					value = page
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if err := json.NewEncoder(w).Encode(value); err != nil {
					t.Error(err)
				}
			}))})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			capture := &validationCapture{required: size != 0, source: func(context.Context, string, string) (tracker.ChangeSourceCapture, error) {
				return source, nil
			}}
			state := validation.VersionState{Change: &runner.NativeChange{ChangeID: detail.Change.ID, VersionID: version.ID}, Data: runtime.Attempt.NativeRunData, Diff: &request, Lease: tracker.NativeLease{WorkItemID: "wi_example"}, Organization: "org_example", Project: "prj_example", SourceRequired: capture.required}
			if size >= tracker.MaxChangeDiffBytes {
				detail.Versions[0].Policy.Gates.SecurityAudit = false
				if err := capture.Retain(t.Context(), request); err != nil {
					t.Fatalf("audit patch limit prevented source retention: %v", err)
				}
				retained := capture.retained
				if retained == nil {
					t.Fatal("source bundle was not retained")
				}
				if err := retained.Source.Validate(base, head, retained.Bundle); err != nil {
					t.Fatal(err)
				}
				if retained.Patch != "" {
					t.Fatal("oversized audit patch must not be represented as complete")
				}
				if input, err := validation.PrepareVersion(t.Context(), native, capture, state, "security_audit"); err != nil || input.Version != nil {
					t.Fatalf("disabled audit rejected retained source: version=%v error=%v", input.Version != nil, err)
				}
				detail.Versions[0].Policy.Gates.SecurityAudit = true
				if input, err := validation.PrepareVersion(t.Context(), native, capture, state, "security_audit"); err == nil || !strings.Contains(err.Error(), "security audit retained source patch is unavailable") || input.Diff != nil {
					t.Fatalf("incomplete audit source was not refused: diff=%v error=%v", input.Diff != nil, err)
				}
				return
			}
			input, err := validation.PrepareVersion(t.Context(), native, capture, state, "security_audit")
			if err != nil {
				t.Fatal(err)
			}
			if input.Diff == nil {
				t.Fatal("audit source missing")
			}
			snapshot, err := connector.SecurityAuditSnapshot("prj_example", connector.Issue{ID: "wi_example"}, *input.Diff)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.BaseSHA != base || snapshot.HeadSHA != head || snapshot.DiffTruncated || sha256.Sum256([]byte(snapshot.Diff)) != expected || size > 0 && !strings.Contains(snapshot.Diff, "+late security change") {
				t.Fatalf("audit lost exact source: captured=%d original=%d", len(snapshot.Diff), len(original))
			}
		})
	}
}

type validationCapture struct {
	required bool
	retained *tracker.ChangeSourceCapture
	source   func(context.Context, string, string) (tracker.ChangeSourceCapture, error)
}

func (c *validationCapture) Retain(ctx context.Context, diff tracker.AttemptDiffRequest) error {
	result, err := changesource.Retain(ctx, c.required, c.retained, diff, c.source)
	if err == nil {
		c.retained = result.Capture
	}
	return err
}
func (c *validationCapture) Retained() *tracker.ChangeSourceCapture             { return c.retained }
func (c *validationCapture) SetDiscussion([]tracker.NativeComment)              {}
func (c *validationCapture) ApplyAuditHumanAction(*tracker.SecurityAuditReview) {}
