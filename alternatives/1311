package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativeChangeLargeValidationFinalization(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	h := newNativeChangeHub(t)
	store, publisher := changeEvidenceArtifactService(t, h)
	for _, test := range []struct {
		name   string
		inline bool
	}{
		{name: "first"},
		{name: "second"},
		{name: "medium receipt without artifact service", inline: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			timings, lines := 1024, 20000
			if test.inline {
				serviceID := h.scheduler.client.artifactServiceID
				t.Cleanup(func() { h.scheduler.client.artifactServiceID = serviceID })
				h.scheduler.client.artifactServiceID = ""
				timings, lines = 180, 1
			}
			issue := h.createInProgress(t, "Large validated change")
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			ctx, stop, err := e.Guard(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			directory := nativeChangeSourceRepo(t)
			readHead := func() string {
				output, err := exec.CommandContext(ctx, "git", "-C", directory, "rev-parse", "HEAD").Output()
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(string(output))
			}
			base := readHead()
			if err := e.PrepareArtifacts(ctx, directory); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte(strings.Repeat("validated change 世界\n", lines)), 0o600); err != nil {
				t.Fatal(err)
			}
			nativeChangeGit(t, directory, "add", "README.md")
			nativeChangeGit(t, directory, "commit", "-m", "large change")
			head := readHead()
			e.SetRepository(nativeChangeRepository)
			e.SetDiffSource(func(ctx context.Context) (tracker.AttemptDiffRequest, bool) {
				diff, err := workspace.GitFileDiffs(ctx, directory, base, tracker.MaxDiffBytes)
				request := tracker.AttemptDiffRequest{BaseSHA: diff.BaseSHA, HeadSHA: diff.HeadSHA}
				for _, file := range diff.Files {
					request.Files = append(request.Files, tracker.AttemptDiffFile{Path: file.Path, Status: file.Status, Additions: file.Additions, Deletions: file.Deletions, Patch: file.Patch})
				}
				return request, err == nil
			})
			if err := e.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			if err := e.Checkpoint(ctx, tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
				t.Fatal(err)
			}
			validation := gate.CommandResult{Command: "make check-land", HeadSHA: head, TreeSHA: strings.Repeat("d", 40), DurationNS: 1, Evidence: &gate.CommandEvidence{}, Output: strings.Repeat("validation passed 世界\n", 2000), Pipeline: &gate.PipelineEvidence{}}
			for i := range timings {
				timing := gate.PipelineTiming{Stage: "finalization", Command: "go test -short -timeout=60s ./internal/example", HeadSHA: head, TreeSHA: validation.TreeSHA, StartedAt: time.Unix(1, int64(i)).UTC(), FinishedAt: time.Unix(2, int64(i)).UTC(), ExitCode: new(0), Execution: "executed"}
				timing.ReceiptID = timing.ID()
				validation.Pipeline.Timings = append(validation.Pipeline.Timings, timing)
			}
			raw, err := json.Marshal(tracker.PublicGateResult(validation))
			if err != nil || len(raw) <= 64<<10 || !validation.Pipeline.Valid() || (len(raw) > operatortool.MaxResultBytes) == test.inline {
				t.Fatalf("fixture has incorrect evidence size or validity: inline=%v bytes=%d error=%v", test.inline, len(raw), err)
			}
			e.validation = &validation
			if err := e.FinalizeArtifacts(ctx, directory); err != nil {
				t.Fatal(err)
			}
			if err := e.Finish(ctx, "succeeded"); err != nil {
				t.Fatal(err)
			}
			change := e.NativeChange()
			if change == nil || change.VersionID == "" || change.Error != "" || change.VersionError != "" || change.VersionCode != "" {
				t.Fatalf("large validated change failed finalization: %+v", change)
			}
			detail, err := h.native.runtimeChange(ctx, tracker.NativeWorkItemID(issue.ID), change.ChangeID)
			if err != nil {
				t.Fatalf("finalized change is unreadable: %v", err)
			}
			if len(detail.Versions) != 1 || detail.Versions[0].ID != change.VersionID || detail.Versions[0].Validation == nil {
				t.Fatal("lost immutable version identity")
			}
			previous := change.VersionID
			if err := e.Finish(ctx, "succeeded"); err != nil || e.NativeChange().VersionID != previous {
				t.Fatalf("finalization replay changed identity: %v", err)
			}
			compact := detail.Versions[0].Validation
			if test.inline {
				expected := tracker.PublicGateResult(validation)
				if !reflect.DeepEqual(*compact, expected) {
					t.Fatal("inline receipt lost validation evidence")
				}
				h.complete(t, issue.ID, change)
				return
			}
			reference := compact.EvidenceArtifact
			if reference == nil || compact.Pipeline != nil || compact.Command != validation.Command || compact.HeadSHA != head || compact.ExitCode != 0 || !reflect.DeepEqual(compact.Evidence, validation.Evidence) {
				t.Fatalf("lost validation identity or artifact: %+v", compact)
			}
			if err := store.PublishPending(ctx, publisher); err != nil {
				t.Fatal(err)
			}
			refs, err := h.native.Artifacts(ctx, tracker.NativeWorkItemID(issue.ID))
			if err != nil {
				t.Fatal(err)
			}
			var foundValidation, foundDiff bool
			for _, ref := range refs {
				if ref.ArtifactID == reference.ArtifactID {
					foundValidation = ref.SHA256 == reference.SHA256
				}
				if ref.Kind == "diff" {
					foundDiff = ref.Bytes > operatortool.MaxResultBytes
				}
			}
			if !foundValidation || !foundDiff {
				t.Fatalf("missing large artifact references: %+v", refs)
			}
			retained := readChangeEvidenceArtifact(t, h.native, tracker.NativeWorkItemID(issue.ID), *reference)
			fullRaw, err := json.Marshal(validation)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(retained, fullRaw) {
				t.Fatal("validation artifact lost complete evidence bytes")
			}
			input := detail.Versions[0].ChangeVersionInput
			for _, field := range []string{"item", "attempt", "digest"} {
				t.Run("refuse foreign or malformed artifact/"+field, func(t *testing.T) {
					altered := *reference
					switch field {
					case "item":
						altered.WorkItemID = artifact.NewID("wi")
					case "attempt":
						altered.AttemptID = artifact.NewID("attempt")
					case "digest":
						altered.SHA256 = "invalid"
					}
					command := *input.Validation
					command.EvidenceArtifact = &altered
					candidate := input
					candidate.Validation = &command
					_, err := h.admin.PublishChangeVersion(ctx, tracker.NativeWorkItemID(issue.ID), change.ChangeID, tracker.PublishChangeVersion{Mutation: nativeMutationKey(), ExpectedVersionID: previous, ChangeVersionInput: candidate})
					if err == nil || hubErrorCode(err) != "invalid_request" {
						t.Fatalf("invalid artifact was not refused: %v", err)
					}
				})
			}
			h.complete(t, issue.ID, change)
		})
	}
}

type changeEvidenceStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (s *changeEvidenceStorage) Put(_ context.Context, key string, data []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[key]; ok {
		return "", artifact.ErrConflict
	}
	s.objects[key] = bytes.Clone(data)
	return "version", nil
}

func (s *changeEvidenceStorage) Get(_ context.Context, key, _ string, limit int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, artifact.ErrMissing
	}
	if int64(len(data)) > limit {
		return nil, artifact.ErrIntegrity
	}
	return bytes.Clone(data), nil
}

func (s *changeEvidenceStorage) Delete(_ context.Context, key, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func changeEvidenceArtifactService(t *testing.T, h *nativeChangeHub) (*artifact.Service, *artifact.RemoteHub) {
	t.Helper()
	var publisher struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := h.admin.client.request(t.Context(), http.MethodPost, "/api/v1/tokens", map[string]string{"name": "artifacts", "scope": "worker"}, &publisher); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.client.request(t.Context(), http.MethodPost, "/api/v2/tokens/"+publisher.ID+"/grants", map[string]any{"organization_id": h.organization, "project_id": h.project}, nil); err != nil {
		t.Fatal(err)
	}
	serviceID := artifact.NewID("service")
	remote := &artifact.RemoteHub{Client: h.admin.client.httpClient, Origin: h.admin.client.baseURL.String(), ServiceID: serviceID, OrganizationID: string(h.organization), ProjectID: string(h.project), PublisherToken: func() string { return publisher.Token }}
	store, err := artifact.NewService(t.Context(), artifact.Config{ServiceID: serviceID, OrganizationID: string(h.organization), Mode: "customer", DatabasePath: filepath.Join(t.TempDir(), "artifacts.db"), Policy: artifact.Policy{ID: "test", Limits: artifact.Limits{RetainedBytes: 64 << 20, ReservedBytes: 32 << 20, ArtifactBytes: 8 << 20, UploadBytes: artifact.MaxChunkBytes, RetentionSeconds: 3600}, AbandonedUploadSeconds: 600, DeletionRecordSeconds: 7200, BackupSeconds: 3600}}, &changeEvidenceStorage{objects: map[string][]byte{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	handler, err := artifact.NewHTTPServer(store, remote)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	binding := artifact.Binding{ServiceID: serviceID, Origin: server.URL, Mode: "customer", PublisherTokenID: publisher.ID}
	if err := h.admin.client.request(t.Context(), http.MethodPut, h.admin.base()+"/artifact-services/"+binding.ServiceID, binding, nil); err != nil {
		t.Fatal(err)
	}
	h.scheduler.client.artifactServiceID, h.scheduler.client.artifactBytes = binding.ServiceID, 4<<20
	return store, remote
}

func readChangeEvidenceArtifact(t *testing.T, client *NativeClient, item tracker.NativeWorkItemID, ref artifact.Reference) []byte {
	t.Helper()
	grant, err := client.ArtifactGrant(t.Context(), item, ref.ArtifactID, ref.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if grant.SHA256 != ref.SHA256 {
		t.Fatal("grant changed immutable manifest identity")
	}
	httpClient := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	read := func(path string, limit int64) []byte {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, grant.Origin+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+grant.Token)
		response, err := httpClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
		if err != nil || response.StatusCode != http.StatusOK || int64(len(data)) > limit {
			t.Fatalf("artifact read failed: status=%d error=%v", response.StatusCode, err)
		}
		return data
	}
	path := fmt.Sprintf("/v1/artifacts/%s/manifests/%d", ref.ArtifactID, ref.Revision)
	raw := read(path, artifact.MaxManifestBytes)
	var manifest artifact.Manifest
	if artifact.Digest(raw) != ref.SHA256 || json.Unmarshal(raw, &manifest) != nil || manifest.Validate() != nil || manifest.Scope != ref.Scope {
		t.Fatal("manifest lost its digest or scope")
	}
	var content []byte
	for _, object := range manifest.Objects {
		data := read(path+"/objects/"+object.ID, object.Size)
		if int64(len(data)) != object.Size || artifact.Digest(data) != object.SHA256 {
			t.Fatal("object lost its digest or size")
		}
		content = append(content, data...)
	}
	return content
}
