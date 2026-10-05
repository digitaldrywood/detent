package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// nativeSSHExecution uses the same bootstrap serialization and bidirectional
// callbacks as a remote worker, with no access to central filesystem paths.
func nativeSSHExecution(t *testing.T, ctx context.Context, execution runner.Execution, journalRoot string) (runner.Execution, func()) {
	t.Helper()
	left, right := net.Pipe()
	request := runner.RunRequest{Execution: execution}
	callbacks := (&runner.Runner{}).SSHRunCallbacks(request)
	central := runner.NewSSHPeer(ctx, left, left, callbacks.Handle)
	sources := &runner.SSHExecutionSources{}
	remote := runner.NewSSHPeer(ctx, right, right, sources.Handle)
	callbacks.BindExecutionSources(central, journalRoot)
	closePeers := func() {
		central.Close()
		remote.Close()
		left.Close()
		right.Close()
	}
	t.Cleanup(closePeers)
	data, err := json.Marshal(runner.NewSSHRunRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	var wire runner.SSHRunRequest
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	wire.Sources = sources
	return wire.Bind(ctx, remote).Execution, closePeers
}

func TestSSHNativeEvidenceSource(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Remote evidence")
	h.claim(t, issue.ID)
	e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	remote, closePeers := nativeSSHExecution(t, t.Context(), e, t.TempDir())
	defer closePeers()
	remote.(runner.EvidenceSourceExecution).SetEvidenceSource(func(_ context.Context, path string) (runner.ValidationEvidence, error) {
		if path != "page.png" {
			return runner.ValidationEvidence{}, os.ErrPermission
		}
		return runner.ValidationEvidence{Name: "page.png", ContentType: "image/png", Content: []byte("remote pixels")}, nil
	})
	file, err := e.evidenceSource(t.Context(), "page.png")
	if err != nil || file.Name != "page.png" || string(file.Content) != "remote pixels" {
		t.Fatalf("remote evidence: %+v, %v", file, err)
	}
	if _, err := e.evidenceSource(t.Context(), "../private.png"); err == nil {
		t.Fatal("remote source refusal was lost")
	}
}

func TestSSHNativePublication(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"complete", "capture disconnected", "upload acknowledgment lost"} {
		t.Run(scenario, func(t *testing.T) {
			h := newNativeChangeHub(t)
			issue := h.createInProgress(t, "Remote diff")
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			ctx, stop, err := e.Guard(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			root, checkout := t.TempDir(), t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				command := exec.CommandContext(ctx, "git", args...)
				command.Dir = checkout
				command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=SSH", "GIT_AUTHOR_EMAIL=ssh@example.test", "GIT_COMMITTER_NAME=SSH", "GIT_COMMITTER_EMAIL=ssh@example.test")
				data, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %s: %v", args, data, err)
				}
				return strings.TrimSpace(string(data))
			}
			git("init", "-b", "main")
			if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("base\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git("add", "README.md")
			git("-c", "commit.gpgsign=false", "commit", "-m", "base")
			base := git("rev-parse", "HEAD")
			parts := map[string][]artifact.Part{}
			lost := scenario == "upload acknowledgment lost"
			store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+h.native.client.tokenSource() {
					t.Error("artifact credential left central ownership")
				}
				if r.URL.Path == "/v1/uploads" {
					var reservation artifact.Reservation
					if err := json.NewDecoder(r.Body).Decode(&reservation); err != nil {
						t.Error(err)
						return
					}
					if reservation.LeaseID != string(e.claim.lease.ID) || reservation.FencingToken != int64(e.claim.lease.FencingToken) || reservation.AttemptID != e.data.AttemptID {
						t.Error("artifact fencing identity changed")
					}
					id := "artifact_" + strings.Repeat("a", 32)
					if reservation.Kind == "diff" {
						id = "artifact_" + strings.Repeat("b", 32)
					}
					_ = json.NewEncoder(w).Encode(artifact.Upload{ArtifactID: id, State: "uploading"})
					return
				}
				id := strings.Split(r.URL.Path, "/")[3]
				if strings.HasSuffix(r.URL.Path, "/parts") {
					var part artifact.Part
					if err := json.NewDecoder(r.Body).Decode(&part); err != nil {
						t.Error(err)
						return
					}
					if part.Sequence < len(parts[id]) {
						if !reflect.DeepEqual(part, parts[id][part.Sequence]) {
							t.Error("artifact retry changed bytes")
						}
					} else {
						parts[id] = append(parts[id], part)
					}
					if lost && strings.HasSuffix(id, "b") {
						lost = false
						w.WriteHeader(503)
						return
					}
					_ = json.NewEncoder(w).Encode(artifact.Object{})
					return
				}
				_ = json.NewEncoder(w).Encode(artifact.Reference{})
			}))
			defer store.Close()
			var publisher struct {
				ID    string `json:"id"`
				Token string `json:"token"`
			}
			if err := h.admin.client.request(ctx, http.MethodPost, "/api/v1/tokens", map[string]string{"name": "artifacts", "scope": "worker"}, &publisher); err != nil {
				t.Fatal(err)
			}
			if err := h.admin.client.request(ctx, http.MethodPost, "/api/v2/tokens/"+publisher.ID+"/grants", map[string]any{"organization_id": h.organization, "project_id": h.project}, nil); err != nil {
				t.Fatal(err)
			}
			binding := artifact.Binding{ServiceID: artifact.NewID("service"), Origin: store.URL, Mode: "customer", PublisherTokenID: publisher.ID}
			if err := h.admin.client.request(ctx, http.MethodPut, h.admin.base()+"/artifact-services/"+binding.ServiceID, binding, nil); err != nil {
				t.Fatal(err)
			}
			h.scheduler.client.artifactServiceID, h.scheduler.client.artifactBytes = binding.ServiceID, 4<<20
			remote, disconnect := nativeSSHExecution(t, ctx, e, root)
			remote.(runner.RepositoryExecution).SetRepository(nativeChangeRepository)
			remote.(runner.DiffExecution).SetDiffSource(func(ctx context.Context) (tracker.AttemptDiffRequest, bool) {
				diff, err := workspace.GitFileDiffs(ctx, checkout, base, tracker.MaxDiffBytes)
				if err != nil {
					return tracker.AttemptDiffRequest{}, false
				}
				request := tracker.AttemptDiffRequest{BaseSHA: diff.BaseSHA, HeadSHA: diff.HeadSHA}
				for _, f := range diff.Files {
					request.Files = append(request.Files, tracker.AttemptDiffFile{Path: f.Path, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions, Patch: f.Patch})
				}
				return request, true
			})
			if err := remote.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			artifacts := remote.(runner.ArtifactExecution)
			if err := artifacts.PrepareArtifacts(ctx, checkout); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("exact remote bytes\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git("add", "README.md")
			git("-c", "commit.gpgsign=false", "commit", "-m", "remote")
			head := git("rev-parse", "HEAD")
			if err := artifacts.ArtifactLog(ctx, "remote log 🌲\n"); err != nil {
				t.Fatal(err)
			}
			if err := remote.Checkpoint(ctx, tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
				t.Fatal(err)
			}
			if scenario == "capture disconnected" {
				disconnect()
				if err := e.FinalizeArtifacts(ctx, checkout); err == nil || !e.artifacts.incomplete || e.artifacts.finished {
					t.Fatalf("lost capture was finalized: %v", err)
				}
				if data, err := os.ReadFile(filepath.Join(e.artifacts.directory, "log")); err != nil || string(data) != "remote log 🌲\n" {
					t.Fatalf("lost journal: %q %v", data, err)
				}
				if _, err := os.Stat(filepath.Join(checkout, ".detent", "artifacts")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("central journal written remotely")
				}
				remote, _ = nativeSSHExecution(t, ctx, e, root)
				artifacts = remote.(runner.ArtifactExecution)
				if err := artifacts.PrepareArtifacts(ctx, checkout); err != nil {
					t.Fatal(err)
				}
			}
			err = artifacts.FinalizeArtifacts(ctx, checkout)
			if scenario == "upload acknowledgment lost" {
				if err == nil {
					t.Fatal("upload failure not injected")
				}
				disconnect()
				// Frozen captures can finish centrally with no SSH connection.
				err = e.FinalizeArtifacts(ctx, "unavailable remote checkout")
			}
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := artifact.CaptureGit(ctx, checkout, base, head, 3)
			if err != nil {
				t.Fatal(err)
			}
			if got := parts["artifact_"+strings.Repeat("b", 32)]; !reflect.DeepEqual(got, bundle.Parts) {
				t.Fatalf("remote artifact changed: %#v", got)
			}
			if got := parts["artifact_"+strings.Repeat("a", 32)]; len(got) != 1 || string(got[0].Data) != "remote log 🌲\n" {
				t.Fatalf("remote log changed: %#v", got)
			}
			if !strings.HasPrefix(e.artifacts.directory, root+string(filepath.Separator)) {
				t.Fatal("journal is not central")
			}
			if err := e.Finish(ctx, "succeeded"); err != nil {
				t.Fatal(err)
			}
			change := e.NativeChange()
			if change == nil || change.VersionID == "" {
				t.Fatalf("no published change: %#v", change)
			}
			detail, err := h.admin.Change(ctx, tracker.NativeWorkItemID(issue.ID), change.ChangeID)
			if err != nil || len(detail.Versions) != 1 || detail.Versions[0].HeadSHA != head || detail.Versions[0].Repository != nativeChangeRepository {
				t.Fatalf("wrong remote version: %#v %v", detail, err)
			}
			if e.lastDiff == nil || e.lastDiff.HeadSHA != head || len(e.lastDiff.Files) != 1 || !strings.Contains(e.lastDiff.Files[0].Patch, "exact remote bytes") {
				t.Fatalf("wrong stored remote diff: %#v", e.lastDiff)
			}
			var stored tracker.AttemptDiff
			if err := h.native.client.request(ctx, http.MethodGet, h.native.base()+"/attempts/"+e.data.AttemptID+"/diff", nil, &stored); err != nil {
				t.Fatal(err)
			}
			if stored.HeadSHA != head || stored.Producer.LeaseID != e.claim.lease.ID || stored.Producer.FencingToken != e.claim.lease.FencingToken || stored.Generation.Seq != e.data.Sequence || len(stored.Files) != 1 || !strings.Contains(stored.Files[0].Patch, "exact remote bytes") {
				t.Fatalf("remote diff lost fencing or bytes: %#v", stored)
			}
			if state := h.state(t, issue.ID); state != "In Progress" {
				t.Fatalf("worker wrote tracker lane: %s", state)
			}
		})
	}
}
