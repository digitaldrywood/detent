package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type fakeCloud struct {
	items        []tracker.NativeIssue
	comments     map[string][]tracker.NativeComment
	writes       []map[string]any
	fail         string
	lost         string
	badPage      string
	dispatchable bool
}

type handlerTransport struct{ handler http.Handler }

func (h handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, req)
	return response.Result(), nil
}

func cloudItem(id string, number int, body string) tracker.NativeIssue {
	return tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org", ProjectID: scheduledCloudProject, WorkItemID: tracker.NativeWorkItemID(id), Number: number}, Body: body, State: "Backlog"}
}

func (f *fakeCloud) command(_ context.Context, name string, args map[string]any, result any) error {
	if args["project_id"] != scheduledCloudProject {
		return errors.New("foreign project")
	}
	if f.fail == name {
		return errors.New("unavailable")
	}
	if f.badPage == name {
		encoded, err := json.Marshal(map[string]any{"project_id": scheduledCloudProject, "reference": args["reference"], "data": map[string]any{}})
		if err != nil {
			return err
		}
		return json.Unmarshal(encoded, result)
	}
	var output any
	switch name {
	case "work_config":
		output = operatortool.WorkReadResult[map[string]any]{ProjectID: scheduledCloudProject, Data: map[string]any{"project": tracker.NativeProject{ID: scheduledCloudProject, OrganizationID: "org", Profile: "native", States: []tracker.NativeState{{Name: "Backlog", Dispatchable: f.dispatchable}}}}}
	case "work_list":
		offset := 0
		if cursor, _ := args["cursor"].(string); cursor != "" {
			offset, _ = strconv.Atoi(cursor)
		}
		page := tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{}}
		if offset < len(f.items) {
			page.Items = append(page.Items, f.items[offset])
		}
		if offset+1 < len(f.items) {
			page.NextCursor = strconv.Itoa(offset + 1)
		}
		output = operatortool.WorkReadResult[tracker.Page[tracker.NativeIssue]]{ProjectID: scheduledCloudProject, Data: page}
	case "work_comments":
		id := args["reference"].(string)
		offset := 0
		if cursor, _ := args["cursor"].(string); cursor != "" {
			offset, _ = strconv.Atoi(cursor)
		}
		page := tracker.Page[tracker.NativeComment]{Items: []tracker.NativeComment{}}
		if offset < len(f.comments[id]) {
			page.Items = append(page.Items, f.comments[id][offset])
		}
		if offset+1 < len(f.comments[id]) {
			page.NextCursor = strconv.Itoa(offset + 1)
		}
		output = operatortool.WorkReadResult[tracker.Page[tracker.NativeComment]]{ProjectID: scheduledCloudProject, Reference: id, Data: page}
	case "file_issue":
		if args["state"] != "Backlog" || !strings.HasPrefix(args["request_id"].(string), "scheduled-create-") {
			return errors.New("filing bypassed intake")
		}
		id := fmt.Sprintf("wi_%d", len(f.items)+1)
		item := cloudItem(id, len(f.items)+1, args["description"].(string))
		f.items = append(f.items, item)
		f.writes = append(f.writes, args)
		output = map[string]any{"resource_id": id}
	case "add_comment":
		id := args["identifier"].(string)
		body := args["body"].(string)
		if f.comments == nil {
			f.comments = map[string][]tracker.NativeComment{}
		}
		f.comments[id] = append(f.comments[id], tracker.NativeComment{OrganizationID: "org", ProjectID: scheduledCloudProject, WorkItemID: tracker.NativeWorkItemID(id), Body: body})
		f.writes = append(f.writes, args)
		output = map[string]any{"resource_id": id}
	default:
		return fmt.Errorf("unexpected tool %s", name)
	}
	if f.lost == name {
		f.lost = ""
		return errors.New("response lost after effect")
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, result)
}

func TestCloudReport(t *testing.T) {
	t.Parallel()
	const diagnostic = "--- FAIL: TestOne (0s)\nFAIL\towner/repo/pkg\t0s"
	const jobs = `[{"id":1,"name":"Coverage","conclusion":"failure","html_url":"coverage-job"},{"id":2,"name":"Race","conclusion":"failure","html_url":"race-job"}]`
	fp := issueorigin.Fingerprint("go-test:owner/repo/pkg:TestOne")
	stamp := issueorigin.Stamp("Imported diagnostic", issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/digitaldrywood/detent/actions/runs/previous", Fingerprint: fp})
	for _, tt := range []struct {
		name, log, lost       string
		imported, green       bool
		legacyReplay          bool
		finalizerFailed       bool
		wantItems, wantWrites int
	}{
		{name: "source problem replay", log: diagnostic, wantItems: 1, wantWrites: 4},
		{name: "lost creation response", log: diagnostic, lost: "file_issue", wantItems: 1, wantWrites: 4},
		{name: "lost occurrence response", log: diagnostic, lost: "add_comment", wantItems: 1, wantWrites: 4},
		{name: "imported comment fingerprint", log: diagnostic, imported: true, wantItems: 2, wantWrites: 4},
		{name: "imported legacy run replay", log: diagnostic, imported: true, legacyReplay: true, wantItems: 2, wantWrites: 2},
		{name: "instance intake replay", log: "network setup failed", wantItems: 2, wantWrites: 4},
		{name: "green imported evidence replay", imported: true, green: true, wantItems: 2, wantWrites: 1},
		{name: "failed finalizer cannot publish green evidence", imported: true, finalizerFailed: true, wantItems: 3, wantWrites: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCloud{lost: tt.lost}
			if tt.imported {
				f.items = []tracker.NativeIssue{cloudItem("wi_unrelated", 1, "Operator work"), cloudItem("wi_imported", 2, "Imported body edited by operator")}
				f.items[1].State = "Blocked"
				f.comments = map[string][]tracker.NativeComment{"wi_imported": {{OrganizationID: "org", ProjectID: scheduledCloudProject, WorkItemID: "wi_imported", Body: "Historical discussion"}, {OrganizationID: "org", ProjectID: scheduledCloudProject, WorkItemID: "wi_imported", Body: stamp}}}
			}
			if tt.legacyReplay {
				for _, link := range []string{"coverage-job", "race-job"} {
					body := issueorigin.Stamp("Original legacy occurrence\n\nJob: "+link, issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/digitaldrywood/detent/actions/runs/1/attempts/1", Fingerprint: fp})
					f.comments["wi_imported"] = append(f.comments["wi_imported"], tracker.NativeComment{OrganizationID: "org", ProjectID: scheduledCloudProject, WorkItemID: "wi_imported", Body: body})
				}
			}
			gh := &fakeGH{logs: map[int64]string{1: tt.log, 2: tt.log}}
			var evidence bytes.Buffer
			input := jobs
			if tt.green {
				input = `[{"id":1,"name":"Coverage","conclusion":"success"}]`
			}
			if tt.finalizerFailed {
				input = `[{"id":1,"name":"Coverage","conclusion":"success"},{"id":0,"name":"Finalize scheduled validation","conclusion":"failure","html_url":"publisher-job"}]`
			}
			for _, run := range []string{"1", "1", "1", "2", "2"} {
				if tt.green && run == "2" {
					continue
				}
				getenv := func(key string) string {
					if key == "GITHUB_RUN_ID" {
						return run
					}
					return scheduledEnv(key)
				}
				destination := &cloudDestination{command: f.command, project: scheduledCloudProject, evidence: &evidence}
				err := reportTo(t.Context(), strings.NewReader(input), gh.command, getenv, destination)
				if err != nil && tt.lost == "" {
					t.Fatal(err)
				}
			}
			if len(f.items) != tt.wantItems || len(f.writes) != tt.wantWrites {
				t.Fatalf("items=%d writes=%d; want %d %d", len(f.items), len(f.writes), tt.wantItems, tt.wantWrites)
			}
			if len(gh.created) != 0 || len(gh.comments) != 0 {
				t.Fatal("native report wrote to GitHub")
			}
			if tt.imported && f.items[1].State != "Blocked" {
				t.Fatal("report changed operator hold")
			}
			for _, args := range f.writes {
				body, comment := args["body"].(string)
				if !comment {
					body = args["description"].(string)
					if args["state"] != "Backlog" {
						t.Fatal("dispatchable source intake")
					}
				}
				if tt.green {
					if !strings.Contains(body, scheduledEnv("CI_DEVELOP_SHA")) || !strings.Contains(body, "does not establish") {
						t.Fatal("green evidence invented completion")
					}
					if args["identifier"] != "wi_imported" {
						t.Fatal("green evidence touched unrelated work")
					}
				} else {
					origin, ok := issueorigin.Parse(body)
					if !ok || !strings.Contains(origin.Source, "/attempts/1") || !strings.Contains(body, scheduledEnv("CI_DEVELOP_SHA")) || !strings.Contains(body, "-job") {
						t.Fatal("native occurrence lost source identity")
					}
					if tt.log == diagnostic && origin.Fingerprint != fp {
						t.Fatal("native fingerprint differs from GitHub")
					}
					if tt.log != diagnostic && !strings.Contains(body, "No source repair is authorized") {
						t.Fatal("unknown infrastructure became source repair")
					}
				}
			}
			if evidence.Len() == 0 {
				t.Fatal("no retained diagnostic payload")
			}
		})
	}
}

func (f *fakeCloud) ListTools(context.Context) ([]operatortool.Definition, error) {
	var definitions []operatortool.Definition
	for _, name := range []string{"work_config", "work_list", "work_comments", "file_issue", "add_comment", "connection_info"} {
		if f.fail == name {
			continue
		}
		definition, _ := operatortool.Lookup(name)
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

func (f *fakeCloud) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return operatortool.Result{}, err
	}
	var output json.RawMessage
	if err := f.command(ctx, call.Name, args, &output); err != nil {
		return operatortool.Result{}, err
	}
	return operatortool.Result{Content: output}, nil
}

func TestCloudTransport(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, fail, badPage    string
		status                 int
		dispatchable           bool
		revokedAfterInitialize bool
	}{
		{name: "scoped connection and paginated discovery"},
		{name: "read only scope", fail: "file_issue"},
		{name: "revoked key", status: http.StatusUnauthorized},
		{name: "revoked key on established session", revokedAfterInitialize: true},
		{name: "narrow project grant", fail: "work_config"},
		{name: "provider unavailable", status: http.StatusServiceUnavailable},
		{name: "Backlog must remain nondispatchable", dispatchable: true},
		{name: "malformed list never means no matches", badPage: "work_list"},
		{name: "malformed comments never lose imported matches", badPage: "work_comments"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCloud{fail: tt.fail, dispatchable: tt.dispatchable, badPage: tt.badPage}
			if tt.badPage == "work_comments" {
				f.items = []tracker.NativeIssue{cloudItem("wi_imported", 1, "Imported work")}
			}
			handler := mcp.NewHTTPHandler(f, "test", mcp.HTTPConfig{Principal: func(*http.Request) operatortool.Identity {
				return operatortool.Identity{PrincipalID: "operator", OrganizationID: "org", CredentialID: "scoped-key"}
			}})
			revoked := false
			client := &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer private-test-key" {
					t.Error("missing private bearer")
				}
				status := tt.status
				if revoked {
					status = http.StatusUnauthorized
				}
				if status != 0 {
					w.WriteHeader(status)
					fmt.Fprint(w, "private-test-key provider diagnostics")
					return
				}
				handler.ServeHTTP(w, r)
			})}}
			transport := &cloudMCP{endpoint: "https://app.detent.cloud/organizations/org/mcp", token: "private-test-key", client: client}
			err := transport.initialize(t.Context())
			revoked = tt.revokedAfterInitialize
			if err == nil {
				destination := &cloudDestination{command: transport.call, project: scheduledCloudProject}
				err = destination.load(t.Context())
			}
			if (err != nil) != (tt.fail != "" || tt.status != 0 || tt.dispatchable || tt.badPage != "" || tt.revokedAfterInitialize) {
				t.Fatalf("transport result %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private-test-key") {
				t.Fatal("secret leaked into diagnostic")
			}
			if transport.session != "" {
				revoked = false
				req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, transport.endpoint, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer private-test-key")
				req.Header.Set("Mcp-Session-Id", transport.session)
				response, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
			}
		})
	}
}

func TestCloudDestinationAuthority(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, repo, endpoint, project, token string
		github                               bool
	}{
		{"other repository keeps GitHub", "owner/other", "", "", "", true},
		{"missing connection", "digitaldrywood/detent", "", scheduledCloudProject, "", false},
		{"missing key", "digitaldrywood/detent", "https://app.detent.cloud/organizations/org/mcp", scheduledCloudProject, "", false},
		{"wrong project", "digitaldrywood/detent", "https://app.detent.cloud/organizations/org/mcp", "prj_other", "private-key", false},
		{"insecure connection", "digitaldrywood/detent", "http://app.detent.cloud/organizations/org/mcp", scheduledCloudProject, "private-key", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string {
				return map[string]string{"GITHUB_REPOSITORY": tt.repo, "DETENT_MCP_URL": tt.endpoint, "DETENT_PROJECT_ID": tt.project, "DETENT_API_KEY": tt.token}[key]
			}
			destination, err := reportingDestination(t.Context(), getenv)
			if tt.github {
				if _, ok := destination.(*githubDestination); err != nil || !ok {
					t.Fatalf("GitHub mode = %v %v", destination, err)
				}
				return
			}
			if err == nil || destination != nil {
				t.Fatal("missing authority fell back to a second tracker")
			}
		})
	}
}
