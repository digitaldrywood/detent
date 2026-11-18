package runnerruntime_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type nativeMachineHeartbeatRequest struct {
	HostMetrics           json.RawMessage                     `json:"host_metrics,omitempty"`
	ChangeCursor          *string                             `json:"change_cursor,omitempty"`
	Admission             *tracker.NativeAdmissionObservation `json:"admission,omitempty"`
	SpriteName            string                              `json:"sprite_name,omitempty"`
	Update                *runnerauth.UpdateObservation       `json:"update,omitempty"`
	CapacityConfig        *runnerauth.CapacityConfig          `json:"capacity_configuration,omitempty"`
	ProjectConfiguration  *runnerauth.ProjectConfiguration    `json:"project_configuration,omitempty"`
	LocalChecks           *runnerauth.LocalChecks             `json:"local_checks,omitempty"`
	Problems              []runnerauth.Problem                `json:"problems"`
	ProtocolMajor         int                                 `json:"protocol_major,omitempty"`
	SettingsRejected      bool                                `json:"settings_rejected,omitempty"`
	BackendIsolation      isolation.Report                    `json:"backend_isolation,omitempty"`
	ProviderReports       []providercapacity.Report           `json:"provider_reports,omitempty"`
	DisplayName           string                              `json:"display_name"`
	Capacity              int                                 `json:"capacity"`
	Version               string                              `json:"version"`
	OS                    string                              `json:"os,omitempty"`
	Architecture          string                              `json:"architecture,omitempty"`
	WorkspaceCapabilities *workspacesession.Capabilities      `json:"workspace_capabilities,omitempty"`
	WorkspaceIsolation    string                              `json:"workspace_isolation,omitempty"`
	Skills                *[]skills.ProviderSkill             `json:"skills,omitempty"`
	CheckoutRepository    *string                             `json:"checkout_repository,omitempty"`
}

func changeTestInput() tracker.ChangeVersionInput {
	return tracker.ChangeVersionInput{BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("a", 40), Repository: "https://github.com/example/repo", Code: changeTestArtifact("code"), Artifacts: []tracker.ChangeArtifact{changeTestArtifact("manifest")}, PolicyID: hubtest.PolicyDescriptor().ID}
}
func changeTestArtifact(kind string) tracker.ChangeArtifact {
	return tracker.ChangeArtifact{Kind: kind, URI: "s3://customer/change/" + kind, SHA256: policy.Digest([]byte(kind)), Availability: "available"}
}
func changeTestSource(t *testing.T, input *tracker.ChangeVersionInput, content ...[]byte) []byte {
	t.Helper()
	bundle, _ := changeTestSourceCheckout(t, input, content...)
	return bundle
}
func changeTestSourceCheckout(t *testing.T, input *tracker.ChangeVersionInput, content ...[]byte) ([]byte, string) {
	t.Helper()
	directory := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory, "-c", "user.name=Source Fixture", "-c", "user.email=source@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("source fixture git %v: %s: %v", args, output, err)
		}
		return string(output)
	}
	git("init", "-b", "main", "--object-format=sha1", "--template=")
	path := filepath.Join(directory, "source.txt")
	if err := os.WriteFile(path, []byte("base source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-m", "base")
	base := strings.TrimSpace(git("rev-parse", "HEAD"))
	data := []byte(input.HeadSHA + "\n")
	if len(content) > 0 && content[0] != nil {
		data = content[0]
		if err := os.WriteFile(path, append(bytes.Clone(data), []byte("draft source\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "source.txt")
		git("commit", "-m", "source draft")
	}
	if len(content) == 0 || content[0] != nil {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "source.txt")
	}
	git("commit", "--allow-empty", "-m", "reviewed source")
	head := strings.TrimSpace(git("rev-parse", "HEAD"))
	bundlePath := filepath.Join(directory, "source.bundle")
	git("bundle", "create", bundlePath, "HEAD", "^"+base)
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	diff := git("diff", "--raw", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv", "-z", base, head, "--")
	input.BaseSHA, input.HeadSHA, input.MergeBaseSHA = base, head, base
	input.Source = &tracker.ChangeSource{
		Format: "git-bundle", BaseSHA: base, HeadSHA: head,
		BundleSHA256: tracker.ChangeSourceDigest(bundle),
		DiffSHA256:   tracker.ChangeSourceDigest([]byte(diff)),
		Bytes:        int64(len(bundle)),
	}
	return bundle, directory
}
