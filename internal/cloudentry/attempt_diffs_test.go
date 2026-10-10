package cloudentry

import (
	"bytes"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/diffbody"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMigratedAttemptDiffReadsThroughEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("durable tenant integration")
	}
	t.Parallel()
	f := newEntryFixture(t)
	store := newSpacesFixture(t, false)
	storage, err := attachment.NewStorage(t.Context(), store.config(), store.transport)
	if err != nil {
		t.Fatal(err)
	}
	f.service.attachments = storage
	alice := newBrowser(t, f.service.Handler())
	alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	_, body := alice.get("/organizations/org_alpha/organization")
	csrf := csrfFrom(t, body)
	project := attachmentProject(t, alice, "org_alpha")
	base := "/api/v2/organizations/org_alpha/projects/" + project
	headers := map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrf}
	created := attachmentRequest(t, alice, http.MethodPost, base+"/work-items", strings.NewReader(`{"idempotency_key":"diff-item","title":"Diff migration"}`), headers)
	if created.Code != http.StatusOK {
		t.Fatalf("issue=%d %s", created.Code, created.Body.String())
	}
	var issue tracker.NativeIssue
	if err := json.Unmarshal(created.Body.Bytes(), &issue); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.fixtures["org_alpha"].path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, file := range []struct{ id, attempt, created, patch string }{
		{"diff_00000000000000000000000000000001", "attempt_00000000000000000000000000000001", time.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339Nano), "@@ old\n+summary survives\n"},
		{"diff_00000000000000000000000000000002", "attempt_00000000000000000000000000000002", time.Now().UTC().Format(time.RFC3339Nano), strings.Repeat("@@ current\n+λ preserved\n", 4000)},
	} {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO attempt_diffs(id,attempt_id,organization_id,project_id,work_item_id,source,source_id,seq,base_sha,head_sha,producer_kind,producer_id,producer_runner_id,producer_lease_id,producer_fencing_token,file_count,patch_bytes,posted_bytes,created_at)
VALUES (?,?,'org_alpha',?,?,'attempt','',2,'base','head','attempt',?,'runner_test','lease_test',1,1,?,?,?)`, file.id, file.attempt, project, issue.WorkItemID, file.attempt, len(file.patch), len(file.patch), file.created); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO attempt_diff_files(diff_id,position,path,status,additions,deletions,patch) VALUES (?,0,'main.go','modified',7,3,?)`, file.id, file.patch); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE attempt_diff_body_migration SET vacuum_pending=1`); err != nil {
		t.Fatal(err)
	}
	organization, err := f.service.registry.Organization(t.Context(), "org_alpha")
	if err != nil {
		t.Fatal(err)
	}
	f.service.attachments = nil
	if _, err := f.service.maintainAttemptDiffBodies(t.Context(), organization); err == nil {
		t.Fatal("migration cleared a body with unavailable storage")
	}
	var retained int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM attempt_diff_files WHERE patch<>''`).Scan(&retained); err != nil || retained != 2 {
		t.Fatalf("failed migration retained %d bodies: %v", retained, err)
	}
	f.service.attachments = storage
	if _, err := f.service.maintainAttemptDiffBodies(t.Context(), organization); err != nil {
		t.Fatal(err)
	}
	var inline int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM attempt_diff_files WHERE patch<>''`).Scan(&inline); err != nil || inline != 0 {
		t.Fatalf("remaining inline bodies=%d: %v", inline, err)
	}
	read := attachmentRequest(t, alice, http.MethodGet, base+"/attempts/attempt_00000000000000000000000000000001/diff", nil, nil)
	var diff tracker.AttemptDiff
	if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &diff) != nil || diff.Files[0].Patch != "@@ old\n+summary survives\n" {
		t.Fatalf("migrated read=%d %s", read.Code, read.Body.String())
	}
	if _, err := f.service.maintainAttemptDiffBodies(t.Context(), organization); err != nil {
		t.Fatal(err)
	}
	read = attachmentRequest(t, alice, http.MethodGet, base+"/attempts/attempt_00000000000000000000000000000001/diff", nil, nil)
	if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &diff) != nil || diff.Files[0].Patch != "" || !diff.Files[0].PatchExpired || diff.Files[0].Additions != 7 || diff.Files[0].Deletions != 3 {
		t.Fatalf("expired read=%d %s", read.Code, read.Body.String())
	}
	read = attachmentRequest(t, alice, http.MethodGet, base+"/work-items/"+string(issue.WorkItemID)+"/diff", nil, nil)
	var current tracker.WorkItemDiff
	if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &current) != nil || current.Diff.Files[0].Patch != strings.Repeat("@@ current\n+λ preserved\n", 4000) {
		t.Fatalf("current read=%d %s", read.Code, read.Body.String())
	}
	keyRequest := `{"name":"diff-read","scope":"read","expires_days":1,"project_ids":["` + project + `"]}`
	created = attachmentRequest(t, alice, http.MethodPost, "/api/v2/organizations/org_alpha/api-keys", strings.NewReader(keyRequest), headers)
	var token struct {
		Token string `json:"token"`
	}
	if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &token) != nil {
		t.Fatalf("token=%d %s", created.Code, created.Body.String())
	}
	call := attachmentMCPClient(t, newBrowser(t, f.service.Handler()), token.Token, mcp.ProtocolVersion)
	for _, name := range []string{operatortool.GetAttemptDiff, operatortool.GetWorkItemDiff} {
		args := map[string]any{"project_id": project, "work_item_id": issue.WorkItemID}
		if name == operatortool.GetAttemptDiff {
			args["attempt_id"] = "attempt_00000000000000000000000000000002"
		}
		raw, failed := call(name, args)
		var result operatortool.ChangeResult
		if failed || json.Unmarshal(raw, &result) != nil || result.Diff == nil || result.Diff.Files[0].Patch != current.Diff.Files[0].Patch {
			t.Fatalf("MCP %s=%s", name, raw)
		}
	}
}

func TestAttemptDiffObjectStorage(t *testing.T) {
	t.Parallel()
	store := newSpacesFixture(t, false)
	storage, err := attachment.NewStorage(t.Context(), store.config(), store.transport)
	if err != nil {
		t.Fatal(err)
	}
	checked := false
	checkStatus := http.StatusNoContent
	s := &Service{attachments: storage, config: Config{
		now:        time.Now,
		Issuer:     "entry",
		SigningKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
		transport: func(Organization) (http.RoundTripper, error) {
			return handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v2/organizations/org_alpha/projects/prj_test/attempts/attempt_test/diff/check" {
					t.Errorf("unexpected check path %s", r.URL.Path)
				}
				checked = true
				w.WriteHeader(checkStatus)
			})}, nil
		},
	}}
	organization := Organization{ID: "org_alpha", Generation: 1}
	path := "/api/v2/organizations/org_alpha/projects/prj_test/attempts/attempt_test/diff"
	request := tracker.AttemptDiffRequest{Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 2}, Files: []tracker.AttemptDiffFile{
		{Path: "main.go", Status: tracker.DiffStatusModified, Additions: 2, Deletions: 1, Patch: "@@ original\n+λ <html>\n"},
		{Path: ".env", Status: tracker.DiffStatusAdded, Additions: 1, Patch: "+PASSWORD=secret"},
	}}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)).WithContext(t.Context()), httptest.NewRecorder())
	c.Request().Header.Set("Authorization", "Bearer fixture")
	claims, err := s.claims(organization, cloudassert.KindMachine, http.MethodPost, path, raw)
	if err != nil {
		t.Fatal(err)
	}
	before := len(store.requests())
	checkStatus = http.StatusForbidden
	if _, status, _, _ := s.uploadAttemptDiff(c, organization, claims, raw); status != http.StatusForbidden || len(store.requests()) != before {
		t.Fatal("refused producer uploaded a diff body")
	}
	checkStatus = http.StatusNoContent
	stored, status, _, err := s.uploadAttemptDiff(c, organization, claims, raw)
	if err != nil || status != 0 || !checked {
		t.Fatalf("upload=%d %v", status, err)
	}
	var uploaded tracker.AttemptDiffRequest
	if err := json.Unmarshal(stored, &uploaded); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "PASSWORD") || strings.Contains(string(stored), "original") || uploaded.PostedBytes != int64(len(request.Files[0].Patch)+len(request.Files[1].Patch)) || uploaded.Files[0].PatchBody == nil || uploaded.Files[1].PatchBody != nil || !uploaded.Files[1].Denied {
		t.Fatalf("metadata retained patch bodies: %s", stored)
	}
	ref := *uploaded.Files[0].PatchBody
	store.failPutResponse = true
	if err := s.storeDiffBody(t.Context(), "org_alpha", "prj_test", ref, "@@ original\n+λ <html>\n"); err != nil {
		t.Fatalf("immutable upload replay: %v", err)
	}
	store.failPutResponse = false
	for _, test := range []struct {
		name, path, tool string
		expired          bool
	}{
		{name: "attempt API", path: path},
		{name: "work item API", path: "/organizations/org_alpha/api/v2/projects/prj_test/work-items/wi_test/diff"},
		{name: "attempt MCP", tool: operatortool.GetAttemptDiff},
		{name: "work item MCP", tool: operatortool.GetWorkItemDiff},
		{name: "expired superseded MCP", tool: operatortool.GetAttemptDiff, expired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			diff := tracker.AttemptDiff{ID: "diff_test", AttemptID: "attempt_test", Files: append([]tracker.AttemptDiffFile{}, uploaded.Files...), FileCount: 2, PatchBytes: ref.Bytes}
			if test.expired {
				diff.Files[0].PatchBody = nil
				diff.Files[0].PatchExpired = true
			}
			var payload any = diff
			requestPath, method := test.path, http.MethodGet
			var call []byte
			if test.tool != "" {
				requestPath, method = "/organizations/org_alpha/mcp", http.MethodPost
				call, err = json.Marshal(map[string]any{"method": "tools/call", "params": map[string]any{"name": test.tool, "arguments": operatortool.ChangeArguments{ProjectID: "prj_test"}}})
				if err != nil {
					t.Fatal(err)
				}
				value, err := json.Marshal(operatortool.ChangeResult{Diff: &diff})
				if err != nil {
					t.Fatal(err)
				}
				payload = map[string]any{"jsonrpc": "2.0", "id": 1, "result": mcp.NewToolCallResult(mcp.ProtocolVersion, value, false)}
			} else if strings.Contains(test.path, "/work-items/") {
				payload = tracker.WorkItemDiff{Diff: &diff}
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
			c := e.NewContext(httptest.NewRequest(method, requestPath, nil).WithContext(t.Context()), httptest.NewRecorder())
			c.Request().Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
			before := len(store.requests())
			if err := s.attemptDiffResponse(c, organization, call, response); err != nil {
				t.Fatal(err)
			}
			result, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			var read tracker.AttemptDiff
			if test.tool != "" {
				var frame struct {
					Result struct {
						Structured operatortool.ChangeResult `json:"structuredContent"`
						Content    []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"result"`
				}
				if err := json.Unmarshal(result, &frame); err != nil {
					t.Fatal(err)
				}
				read = *frame.Result.Structured.Diff
				var text operatortool.ChangeResult
				if err := json.Unmarshal([]byte(frame.Result.Content[0].Text), &text); err != nil {
					t.Fatal(err)
				}
				if text.Diff.Files[0].Patch != read.Files[0].Patch {
					t.Fatal("MCP text and structured content differ")
				}
			} else if strings.Contains(test.path, "/work-items/") {
				var value tracker.WorkItemDiff
				if err := json.Unmarshal(result, &value); err != nil {
					t.Fatal(err)
				}
				read = *value.Diff
			} else if err := json.Unmarshal(result, &read); err != nil {
				t.Fatal(err)
			}
			want := "@@ original\n+λ <html>\n"
			if test.expired {
				want = ""
				if len(store.requests()) != before {
					t.Fatal("expired read accessed object storage")
				}
			}
			if read.Files[0].Patch != want || read.Files[0].Additions != 2 || read.Files[0].Deletions != 1 || read.Files[0].PatchBody != nil || read.Files[0].PatchExpired != test.expired {
				t.Fatalf("read=%+v", read.Files[0])
			}
		})
	}
	for _, test := range []struct {
		name    string
		ref     diffbody.Reference
		content string
	}{
		{name: "missing", ref: diffbody.New("org_alpha", "prj_test", "diff_missing", 0, "missing")},
		{name: "corrupt", ref: ref, content: "corrupted"},
		{name: "foreign organization", ref: diffbody.New("org_beta", "prj_test", "diff_foreign", 0, "foreign")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.content != "" {
				store.mu.Lock()
				store.objects[ref.Key] = spacesObject{content: []byte(test.content)}
				store.mu.Unlock()
			}
			if _, err := s.readDiffBody(t.Context(), "org_alpha", "prj_test", test.ref); err == nil {
				t.Fatal("unavailable body was returned as a successful diff")
			}
		})
	}
	t.Run("pending vacuum does not run online", func(t *testing.T) {
		calls := 0
		s.config.transport = func(Organization) (http.RoundTripper, error) {
			return handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch r.URL.Path {
				case "/internal/v1/diff-bodies/batch":
					if err := json.NewEncoder(w).Encode(diffbody.Batch{VacuumPending: true}); err != nil {
						t.Error(err)
					}
				default:
					t.Errorf("unexpected maintenance path %s", r.URL.Path)
				}
			})}, nil
		}
		active, err := s.maintainAttemptDiffBodies(t.Context(), organization)
		if err != nil || active || calls != 1 {
			t.Fatalf("pending maintenance active=%v calls=%d err=%v", active, calls, err)
		}
	})
}
