package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/releaseaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReleaseAuditProjectRecords(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
	first := newNativeFixture(t, service, "", "audit-first")
	second := newNativeFixture(t, service, first.project.OrganizationID, "audit-second")
	blobs := map[string][]byte{}
	metadata := map[string]attachment.Metadata{}
	for _, fixture := range []nativeFixture{first, second} {
		var uploader string
		if err := service.database.reader.QueryRowContext(t.Context(), "SELECT token_id FROM token_grants WHERE project_id=? LIMIT 1", fixture.project.ID).Scan(&uploader); err != nil {
			t.Fatal(err)
		}
		if _, err := service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository=? WHERE id=?", "example-org/example-project", fixture.project.ID); err != nil {
			t.Fatal(err)
		}
		for sequence := 1; sequence <= 2; sequence++ {
			state := releaseaudit.Snapshot{Identity: releaseaudit.Identity{ProjectID: string(fixture.project.ID), Repository: "example-org/example-project", Commit: strings.Repeat("a", 40), ReleaseSeries: "release/v1.0", RunID: "run", Attempt: "1", JobID: "audit", RunURL: "https://example.test/runs/1"}, Sequence: sequence, Status: "incomplete", BundleCommit: releaseaudit.BundleCommit, BundleDigest: releaseaudit.BundleDigest, Allowance: releaseaudit.Allowance{Invocations: 20, Usage: releaseaudit.Usage{Input: 1000, Output: 1000, Cached: 1000}}, Ledger: []byte(`[]`)}
			raw, checkpoint, err := releaseaudit.SnapshotBytes(state)
			if err != nil {
				t.Fatal(err)
			}
			id := conversation.NewAttachmentID()
			digest := sha256.Sum256(raw)
			blobs[id] = raw
			metadata[id] = attachment.Metadata{ID: id, ProjectID: string(fixture.project.ID), Uploader: uploader, Name: "audit.txt", ContentType: "text/plain", Size: int64(len(raw)), SHA256: hex.EncodeToString(digest[:]), CreatedAt: now}
			if _, err := service.database.db.ExecContext(t.Context(), `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, fixture.project.OrganizationID, fixture.project.ID, uploader, "audit.txt", "text/plain", len(raw), hex.EncodeToString(digest[:]), formatHubTime(now.Add(-48*time.Hour))); err != nil {
				t.Fatal(err)
			}
			checkpoint.Parts = []releaseaudit.Part{{AttachmentID: id, Index: 0, Offset: 0, Bytes: len(raw), Digest: hex.EncodeToString(digest[:])}}
			request := struct {
				tracker.Mutation
				releaseaudit.Checkpoint
			}{tracker.Mutation{IdempotencyKey: fmt.Sprintf("audit-%s-%d", fixture.project.ID, sequence)}, checkpoint}
			response := performHubAPIRequest(t, service, http.MethodPost, fixture.base+"/release-audits", fixture.token, request)
			requireNativeStatus(t, response, http.StatusOK)
			var retained releaseaudit.Checkpoint
			decodeHubResponse(t, response, &retained)
			if !reflect.DeepEqual(retained, checkpoint) {
				t.Fatalf("checkpoint=%+v want=%+v", retained, checkpoint)
			}
			request.IdempotencyKey += "-replay"
			response = performHubAPIRequest(t, service, http.MethodPost, fixture.base+"/release-audits", fixture.token, request)
			requireNativeStatus(t, response, http.StatusOK)
			request.IdempotencyKey += "-conflict"
			request.Status = "clean"
			response = performHubAPIRequest(t, service, http.MethodPost, fixture.base+"/release-audits", fixture.token, request)
			requireNativeStatus(t, response, http.StatusConflict)
		}
	}
	if err := service.maintainNativeRetention(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []nativeFixture{first, second} {
		response := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/release-audits", fixture.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page releaseaudit.CheckpointPage
		decodeHubResponse(t, response, &page)
		if len(page.Items) != 1 || page.Items[0].Sequence != 2 || page.Items[0].Identity.ProjectID != string(fixture.project.ID) {
			t.Fatalf("history crossed project or returned stale state: %+v", page)
		}
		for _, part := range page.Items[0].Parts {
			var deleted bool
			if err := service.database.reader.QueryRowContext(t.Context(), "SELECT deleted_at IS NOT NULL FROM attachments WHERE id=?", part.AttachmentID).Scan(&deleted); err != nil || deleted {
				t.Fatalf("audit evidence expired: %v %v", deleted, err)
			}
		}
	}
	response := performHubAPIRequest(t, service, http.MethodGet, second.base+"/release-audits", first.token, nil)
	requireNativeStatus(t, response, http.StatusNotFound)
	var uploader string
	if err := service.database.reader.QueryRowContext(t.Context(), "SELECT token_id FROM token_grants WHERE project_id=? LIMIT 1", first.project.ID).Scan(&uploader); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, first.base+"/attachments") {
			service.Handler().ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+first.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(io.LimitReader(r.Body, 32769))
			if err != nil || len(body) > 32768 {
				http.Error(w, "invalid part", http.StatusBadRequest)
				return
			}
			id := conversation.NewAttachmentID()
			digest := sha256.Sum256(body)
			record := attachment.Metadata{ID: id, ProjectID: string(first.project.ID), Uploader: uploader, Name: r.Header.Get("X-Attachment-Name"), ContentType: r.Header.Get("Content-Type"), Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:]), CreatedAt: now}
			record.Reference = record.Markdown(string(first.project.OrganizationID))
			_, err = service.database.db.ExecContext(r.Context(), `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, first.project.OrganizationID, first.project.ID, uploader, record.Name, record.ContentType, record.Size, record.SHA256, formatHubTime(now))
			if err != nil {
				t.Error(err)
				http.Error(w, "storage failed", http.StatusInternalServerError)
				return
			}
			blobs[id], metadata[id] = body, record
			if err := json.NewEncoder(w).Encode(record); err != nil {
				t.Error(err)
			}
			return
		}
		id := strings.TrimPrefix(r.URL.Path, first.base+"/attachments/")
		body, ok := blobs[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		result := operatortool.AttachmentContentResult{Metadata: metadata[id], ContentBase64: base64.StdEncoding.EncodeToString(body), Offset: 0, ReturnedBytes: len(body), MaxContentBytes: operatortool.AttachmentContentBytes, EOF: true}
		if err := json.NewEncoder(w).Encode(result); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := hubclient.New(hubclient.Config{URL: server.URL, HTTPClient: server.Client(), TokenSource: func() string { return first.token }})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(first.project.OrganizationID, first.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	provider := &hubAuditProvider{t: t}
	stage, err := releaseaudit.New(provider, native.ReleaseAuditRecords(), hubAuditReporter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"first", "second"} {
		provider.scratch = t.TempDir()
		result, err := stage.Run(t.Context(), releaseaudit.Request{Identity: releaseaudit.Identity{ProjectID: string(first.project.ID), Repository: "example-org/example-project", Commit: strings.Repeat("a", 40), ReleaseSeries: "release/v1.0", RunID: run, Attempt: "1", JobID: "audit", RunURL: "https://example.test/runs/" + run}, Scratch: provider.scratch, Source: hubAuditSource{}, Allowance: releaseaudit.Allowance{Invocations: 20, Usage: releaseaudit.Usage{Input: 1000, Output: 1000}}})
		if err != nil || result.Status != "needs_validation" || len(result.Findings) != 1 {
			t.Fatalf("stage through Hub: %s %v", result.Status, err)
		}
	}
	if !provider.priorSeen {
		t.Fatal("the second stage did not hydrate the first stage's Hub records")
	}
}

type hubAuditProvider struct {
	t         *testing.T
	scratch   string
	sessions  int
	priorSeen bool
}

func (p *hubAuditProvider) Invoke(ctx context.Context, invocation releaseaudit.Invocation, observe func(releaseaudit.Usage) error) (releaseaudit.Response, error) {
	p.t.Helper()
	if !strings.Contains(invocation.Prompt, releaseaudit.UntrustedContentRule) || !strings.Contains(invocation.Prompt, releaseaudit.BundleCommit) {
		p.t.Fatal("audit omitted the pinned skill or untrusted-content rule")
	}
	relative, err := filepath.Rel(p.scratch, invocation.Scratch)
	if err != nil || !filepath.IsLocal(relative) {
		p.t.Fatal("audit escaped job scratch")
	}
	source, err := invocation.Source.Read(ctx, "source.go")
	if err != nil || string(source) != "package example" || invocation.Source.Commit() != strings.Repeat("a", 40) {
		p.t.Fatal("audit read a different pinned source")
	}
	p.sessions++
	if invocation.Phase == "recon-prior-evidence" {
		var input struct {
			Task []releaseaudit.Snapshot `json:"task"`
		}
		if err := json.Unmarshal(invocation.Input, &input); err != nil {
			p.t.Fatal(err)
		}
		for _, previous := range input.Task {
			if previous.Identity.RunID == "first" {
				p.priorSeen = len(previous.Findings) == 1 && len(previous.Ledger) > 2 && strings.Contains(string(previous.Findings[0]), `"verdict":"needs_validation"`)
			}
		}
	}
	if err := observe(releaseaudit.Usage{Input: 1, Output: 1}); err != nil {
		return releaseaudit.Response{}, err
	}
	output := json.RawMessage(`{}`)
	switch invocation.Phase {
	case "recon-ledger":
		output = hubAuditLedger("planned")
	case "hunt":
		output, err = json.Marshal(struct {
			Ledger     json.RawMessage          `json:"ledger"`
			Candidates []releaseaudit.Candidate `json:"candidates"`
		}{hubAuditLedger("candidate"), []releaseaudit.Candidate{{Fingerprint: "source.go:change:missing-ownership", CoverageID: "route::owner::api::access-control", Record: json.RawMessage(hubAuditFinding)}}})
		if err != nil {
			return releaseaudit.Response{}, err
		}
	case "coverage-critic", "final-coverage-critic":
		output = hubAuditLedger("candidate")
	case "candidate-disprover":
		output = json.RawMessage(hubAuditFinding)
	case "independent-record-verifier", "replacement-record-verifier":
		var input struct {
			Task struct {
				Record json.RawMessage `json:"record"`
				Digest string          `json:"record_sha256"`
			} `json:"task"`
		}
		if err := json.Unmarshal(invocation.Input, &input); err != nil {
			p.t.Fatal(err)
		}
		if len(input.Task.Record) == 0 || input.Task.Digest != fmt.Sprintf("%x", sha256.Sum256(input.Task.Record)) {
			p.t.Fatal("verification did not bind the exact report to its digest")
		}
		output, err = json.Marshal(struct {
			Decision string `json:"decision"`
			Digest   string `json:"record_sha256"`
		}{"accept", input.Task.Digest})
		if err != nil {
			return releaseaudit.Response{}, err
		}
	}
	return releaseaudit.Response{SessionID: strconv.Itoa(p.sessions), Output: output}, nil
}

type hubAuditSource struct{}

func (hubAuditSource) Commit() string                          { return strings.Repeat("a", 40) }
func (hubAuditSource) Paths(context.Context) ([]string, error) { return []string{"source.go"}, nil }
func (hubAuditSource) Read(context.Context, string) ([]byte, error) {
	return []byte("package example"), nil
}

type hubAuditReporter struct{}

func (hubAuditReporter) Confirmed(context.Context, releaseaudit.Identity, string, json.RawMessage) error {
	return errors.New("unexpected repair")
}
func (hubAuditReporter) Infrastructure(context.Context, releaseaudit.Identity, string) error {
	return nil
}

const hubAuditFinding = `{"verdict":"needs_validation","fingerprint":"source.go:change:missing-ownership","title":"Missing ownership check","description":"Another user's object may change.","claimed_root_cause":"The operation may omit ownership.","trace":[{"kind":"entrypoint","file":"source.go","line":1,"scope":"change","description":"A user selects an object."},{"kind":"sink","file":"source.go","line":1,"scope":"change","description":"The object may change without ownership."}],"evidence":[{"file":"source.go","line":1,"description":"The ownership boundary needs reproduction."}],"blockers":["The target execution sandbox is unavailable."],"validation_plan":{"local":"Reproduce the ownership check in an isolated fixture."}}`

func hubAuditLedger(status string) json.RawMessage {
	owner, paths, checks, fingerprints := "null", "[]", "[]", "[]"
	if status == "candidate" {
		owner = `"agent-0006"`
		paths = `["source.go"]`
		checks = `[{"agent_id":"agent-0006","reviewed_paths":["source.go"],"invariant":"Only the owner changes an object.","method":"source","result":"The boundary needs reproduction.","artifact":null}]`
		fingerprints = `["source.go:change:missing-ownership"]`
	}
	return json.RawMessage(fmt.Sprintf(`[{"coverage_id":"route::owner::api::access-control","canonical_refs":{"surface":"route","boundary":"owner","subsystem":"api","attack_class":"access-control"},"surface":"Route","boundary":"Object ownership","subsystem":"API","attack_class":"Access control","starting_paths":["source.go"],"ordinary_attack_class_block":"ATTACK-CLASSES.md#Access control","selected_companion_blocks":[],"excluded_blocks":[],"prior_status":"new","attempts":[],"wave":1,"status":"%s","agent_id":%s,"reviewed_paths":%s,"local_checks":%s,"result_fingerprints":%s,"unresolved":[]}]`, status, owner, paths, checks, fingerprints))
}
