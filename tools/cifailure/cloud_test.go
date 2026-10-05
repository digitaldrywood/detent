package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type fakeCloud struct {
	items         []tracker.NativeIssue
	comments      map[string][]tracker.NativeComment
	writes        []map[string]any
	fail          string
	lost          string
	badPage       string
	summaryDetail bool
	states        []tracker.NativeState
	conflict      bool
	editAttempts  int
}

type handlerTransport struct{ handler http.Handler }

func (h handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, req)
	return response.Result(), nil
}

func cloudItem(id string, number int, body string) tracker.NativeIssue {
	return tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org", ProjectID: scheduledCloudProject, WorkItemID: tracker.NativeWorkItemID(id), Number: number, Revision: 7}, Body: body, State: "Backlog"}
}

func (f *fakeCloud) command(_ context.Context, name string, args map[string]any, result any) error {
	if name == "edit_item" {
		f.editAttempts++
	}
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
		states := f.states
		if states == nil {
			states = []tracker.NativeState{{Name: "Backlog"}, {Name: "Todo", Dispatchable: true}}
		}
		output = operatortool.WorkReadResult[map[string]any]{ProjectID: scheduledCloudProject, Data: map[string]any{"project": tracker.NativeProject{ID: scheduledCloudProject, OrganizationID: "org", Profile: "native", States: states}}}
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
		output = operatortool.WorkReadResult[tracker.Page[operatortool.NativeItem]]{ProjectID: scheduledCloudProject, Data: operatortool.NativeItemPage(scheduledCloudProject, page)}
	case "work_item":
		id := args["reference"].(string)
		for _, item := range f.items {
			if string(item.WorkItemID) == id {
				if f.summaryDetail {
					item = operatortool.NativeListIssue(item)
				}
				output = operatortool.WorkReadResult[operatortool.NativeItem]{ProjectID: scheduledCloudProject, Reference: id, Data: operatortool.NativeItemView(scheduledCloudProject, item)}
				break
			}
		}
		if output == nil {
			return errors.New("unknown work item")
		}
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
		if encoded, err := json.Marshal(args); err != nil || len(encoded) > 64<<10 {
			return errors.New("scheduled creation exceeds the native request bound")
		}
		if args["state"] != "Todo" || !strings.HasPrefix(args["request_id"].(string), "scheduled-create-") {
			return errors.New("filing bypassed reporting policy")
		}
		id := fmt.Sprintf("wi_%d", len(f.items)+1)
		item := cloudItem(id, len(f.items)+1, args["description"].(string))
		item.State = args["state"].(string)
		item.Labels = args["labels"].([]string)
		if raw, present := args["priority"]; present {
			encoded, err := json.Marshal(raw)
			var rank int
			if err != nil || json.Unmarshal(encoded, &rank) != nil || rank < 1 || rank > 4 {
				return errors.New("invalid creation priority")
			}
			level := rank - 1
			item.Priority = &level
		}
		f.items = append(f.items, item)
		f.writes = append(f.writes, args)
		output = map[string]any{"resource_id": id, "revision": fmt.Sprint(item.Revision)}
	case "edit_item":
		fields := map[string]any{}
		for key, value := range args {
			if key != "request_id" {
				fields[key] = value
			}
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		request, err := operatortool.DecodeWorkArguments(operatortool.EditItem, encoded)
		if err != nil {
			return err
		}
		if request.Priority == nil && request.Labels == nil || !strings.HasPrefix(args["request_id"].(string), "scheduled-priority-") {
			return errors.New("failure edit has no priority or labels")
		}
		for i := range f.items {
			item := &f.items[i]
			if string(item.WorkItemID) != request.Identifier {
				continue
			}
			if f.conflict {
				item.Revision++
			}
			if int64(item.Revision) != request.ExpectedRevision {
				return errors.New("revision conflict")
			}
			if request.Priority != nil {
				item.Priority = request.Priority
			}
			if request.Labels != nil {
				item.Labels = *request.Labels
			}
			item.Revision++
			f.writes = append(f.writes, args)
			output = map[string]any{"resource_id": request.Identifier, "revision": fmt.Sprint(item.Revision)}
			break
		}
		if output == nil {
			return errors.New("unknown priority target")
		}
	case "move_item":
		encoded, err := json.Marshal(args)
		if err != nil {
			return err
		}
		request, err := operatortool.DecodeNativeMoveItem(encoded)
		if err != nil || request.TargetState != "Todo" || !strings.HasPrefix(request.RequestID, "scheduled-todo-") {
			return errors.New("invalid scheduled transition")
		}
		for i := range f.items {
			item := &f.items[i]
			if string(item.WorkItemID) != request.Identifier {
				continue
			}
			if int64(item.Revision) != request.ExpectedRevision || item.State != "Backlog" {
				return errors.New("transition conflict")
			}
			item.State = request.TargetState
			item.Revision++
			f.writes = append(f.writes, args)
			output = map[string]any{"data": item, "resource_id": request.Identifier, "revision": int64(item.Revision)}
			break
		}
		if output == nil {
			return errors.New("unknown transition target")
		}
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
	const lintDiagnostic = "internal/one.go:12:3: unused value (staticcheck)"
	gosecDiagnostic := fixture(t, "gosec")
	const jobs = `[{"id":1,"name":"Coverage","conclusion":"failure","html_url":"coverage-job"},{"id":2,"name":"Race","conclusion":"failure","html_url":"race-job"}]`
	fp := issueorigin.Fingerprint("go-test:owner/repo/pkg:TestOne")
	stamp := issueorigin.Stamp("Scheduled validation job **Coverage** (failure) failed on development commit previous.\nImported diagnostic", issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/digitaldrywood/detent/actions/runs/previous", Fingerprint: fp})
	var verbose strings.Builder
	for i := range 12 {
		fmt.Fprintf(&verbose, "--- FAIL: TestVerbose%d (0s)\n%s\n", i, strings.Repeat("diagnostic output ", 500))
	}
	verbose.WriteString("FAIL\towner/repo/pkg\t0s")
	for _, tt := range []struct {
		name, log, laterLog, lost, fail string
		imported, green, foreign        bool
		legacyReplay                    bool
		markerReplay                    bool
		finalizerFailed                 bool
		priority                        *int
		holdState                       string
		missingRevision                 bool
		conflict                        bool
		wantItems, wantWrites           int
		wantEdits, wantErrors           int
	}{
		{name: "source problem replay", log: diagnostic, wantItems: 2, wantWrites: 4},
		{name: "source lint replay", log: lintDiagnostic, wantItems: 2, wantWrites: 4},
		{name: "recorded gosec replay", log: gosecDiagnostic, wantItems: 2, wantWrites: 4},
		{name: "gosec cache keeps instance intake", log: strings.ReplaceAll(gosecDiagnostic, "/home/runner/work/detent/detent/", "/runner/cache/tool/"), wantItems: 2, wantWrites: 4},
		{name: "lost creation response", log: diagnostic, lost: "file_issue", wantItems: 2, wantWrites: 4, wantErrors: 1},
		{name: "lost occurrence response", log: diagnostic, lost: "add_comment", wantItems: 2, wantWrites: 4, wantErrors: 1},
		{name: "foreign scheduled job is not reused", log: diagnostic, imported: true, foreign: true, wantItems: 4, wantWrites: 4},
		{name: "imported comment fingerprint", log: diagnostic, imported: true, wantItems: 3, wantWrites: 5, wantEdits: 1},
		{name: "Backlog intake enters Todo", log: diagnostic, imported: true, holdState: "Backlog", wantItems: 3, wantWrites: 6, wantEdits: 1},
		{name: "Human Review retains lane and Urgent", log: diagnostic, imported: true, holdState: "Human Review", priority: new(0), wantItems: 3, wantWrites: 4},
		{name: "normal imported priority", log: diagnostic, imported: true, priority: new(2), wantItems: 3, wantWrites: 5, wantEdits: 1},
		{name: "low imported priority", log: diagnostic, imported: true, priority: new(3), wantItems: 3, wantWrites: 5, wantEdits: 1},
		{name: "High imported priority", log: diagnostic, imported: true, priority: new(1), wantItems: 3, wantWrites: 4},
		{name: "Urgent imported priority", log: diagnostic, imported: true, priority: new(0), wantItems: 3, wantWrites: 4},
		{name: "lost priority response", log: diagnostic, imported: true, lost: "edit_item", wantItems: 3, wantWrites: 5, wantEdits: 1, wantErrors: 1},
		{name: "stale revision stops publication", log: diagnostic, imported: true, conflict: true, wantItems: 3, wantWrites: 2, wantEdits: 5, wantErrors: 5},
		{name: "missing revision stops publication", log: diagnostic, imported: true, missingRevision: true, wantItems: 3, wantWrites: 2, wantErrors: 5},
		{name: "priority authority denied", log: diagnostic, imported: true, fail: "edit_item", wantItems: 3, wantWrites: 2, wantEdits: 5, wantErrors: 5},
		{name: "imported legacy run replay", log: diagnostic, imported: true, legacyReplay: true, wantItems: 2, wantWrites: 3, wantEdits: 1},
		{name: "imported marker replay promotes priority", log: diagnostic, imported: true, legacyReplay: true, markerReplay: true, wantItems: 2, wantWrites: 3, wantEdits: 1},
		{name: "verbose findings stay bounded", log: verbose.String(), wantItems: 2, wantWrites: 4},
		{name: "multiple findings stay in one job issue", log: diagnostic + "\n--- FAIL: TestTwo (0s)\nFAIL\towner/repo/pkg\t0s", wantItems: 2, wantWrites: 4},
		{name: "changed findings reuse job", log: diagnostic, laterLog: "--- FAIL: TestTwo (0s)\nFAIL\towner/repo/pkg\t0s", wantItems: 2, wantWrites: 4},
		{name: "unclassified then source reuses job", log: "exit code 1", laterLog: diagnostic, wantItems: 2, wantWrites: 4},
		{name: "imported infra labelled and promoted", log: "The runner has received a shutdown signal", imported: true, holdState: "Backlog", priority: new(0), wantItems: 3, wantWrites: 6, wantEdits: 1},
		{name: "lost transition response", log: diagnostic, imported: true, holdState: "Backlog", lost: "move_item", wantItems: 3, wantWrites: 6, wantEdits: 1, wantErrors: 1},
		{name: "transition authority denied", log: diagnostic, imported: true, holdState: "Backlog", fail: "move_item", wantItems: 3, wantWrites: 3, wantEdits: 1, wantErrors: 5},
		{name: "instance intake replay", log: "network setup failed", wantItems: 2, wantWrites: 4},
		{name: "authentication remains intake", log: "HTTP 401: authentication required", wantItems: 2, wantWrites: 4},
		{name: "backend startup remains intake", log: "backend startup failed: protocol handshake error", wantItems: 2, wantWrites: 4},
		{name: "green imported evidence replay", imported: true, green: true, wantItems: 2, wantWrites: 1},
		{name: "failed finalizer cannot publish green evidence", imported: true, finalizerFailed: true, wantItems: 3, wantWrites: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCloud{lost: tt.lost, fail: tt.fail, conflict: tt.conflict}
			if tt.imported {
				f.items = []tracker.NativeIssue{cloudItem("wi_unrelated", 1, "Operator work"), cloudItem("wi_imported", 2, "Imported body edited by operator")}
				f.items[1].State = "Blocked"
				if tt.holdState != "" {
					f.items[1].State = tt.holdState
				}
				f.items[1].Priority = tt.priority
				f.items[1].Labels = []string{"operator-label"}
				if tt.missingRevision {
					f.items[1].Revision = 0
				}
				importedStamp := stamp
				if tt.foreign {
					importedStamp = strings.ReplaceAll(stamp, "digitaldrywood/detent/actions", "owner/other/actions")
				}
				f.comments = map[string][]tracker.NativeComment{"wi_imported": {{OrganizationID: "org", ProjectID: scheduledCloudProject, WorkItemID: "wi_imported", Body: "Historical discussion"}, {OrganizationID: "org", ProjectID: scheduledCloudProject, WorkItemID: "wi_imported", Body: importedStamp}}}
			}
			if tt.legacyReplay {
				for i, link := range []string{"coverage-job", "race-job"} {
					body := issueorigin.Stamp("Scheduled validation job **"+[]string{"Coverage", "Race"}[i]+"** (failure) failed on development commit previous.\n\nJob: "+link, issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/digitaldrywood/detent/actions/runs/1/attempts/1", Fingerprint: fp})
					if tt.markerReplay {
						getenv := func(key string) string {
							if key == "GITHUB_RUN_ID" {
								return "1"
							}
							return scheduledEnv(key)
						}
						body += "\n\n" + occurrenceMarker(occurrenceKey(getenv, job{ID: int64(i + 1), Name: []string{"Coverage", "Race"}[i]}, fp))
					}
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
			errorCount := 0
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
				if run == "2" && tt.laterLog != "" {
					gh.logs = map[int64]string{1: tt.laterLog, 2: tt.laterLog}
				}
				destination := &cloudDestination{command: f.command, project: scheduledCloudProject, evidence: &evidence}
				err := reportTo(t.Context(), strings.NewReader(input), gh.command, getenv, destination)
				if err != nil {
					errorCount++
					if tt.wantErrors == 0 {
						t.Fatal(err)
					}
				}
			}
			if errorCount != tt.wantErrors || f.editAttempts != tt.wantEdits {
				t.Fatalf("errors=%d edits=%d; want %d %d", errorCount, f.editAttempts, tt.wantErrors, tt.wantEdits)
			}
			if len(f.items) != tt.wantItems || len(f.writes) != tt.wantWrites {
				t.Fatalf("items=%d writes=%d; want %d %d", len(f.items), len(f.writes), tt.wantItems, tt.wantWrites)
			}
			if len(gh.created) != 0 || len(gh.comments) != 0 {
				t.Fatal("native report wrote to GitHub")
			}
			if tt.imported {
				state := tt.holdState
				if state == "" {
					state = "Blocked"
				} else if state == "Backlog" && tt.fail != "move_item" {
					state = "Todo"
				}
				if f.items[1].State != state {
					t.Fatal("report changed operator hold")
				}
			}
			for i, item := range f.items {
				var want *int
				if !tt.imported || i > 1 {
					want = new(1)
				} else if i == 1 {
					want = tt.priority
					if tt.wantEdits == 1 && (want == nil || *want > 1) {
						want = new(1)
					}
				}
				if (item.Priority == nil) != (want == nil) || want != nil && *item.Priority != *want {
					t.Fatalf("item %s priority=%v; want %v", item.WorkItemID, item.Priority, want)
				}
			}
			if tt.imported && !tt.foreign && (f.items[1].Body != "Imported body edited by operator" || f.comments["wi_imported"][1].Body != stamp) {
				t.Fatal("priority changed imported content or history")
			}
			for _, args := range f.writes {
				if target, move := args["target_state"]; move {
					if target != "Todo" || args["identifier"] != "wi_imported" || args["expected_revision"] != int64(8) {
						t.Fatal("transition lost its native revision or target")
					}
					continue
				}
				if _, edit := args["expected_revision"]; edit {
					fingerprint := legacyJobFingerprint("scheduled-ci:digitaldrywood/detent:Coverage")
					key := issueorigin.Fingerprint("digitaldrywood/detent:1:1:1:Coverage:" + fingerprint)
					if args["expected_revision"] != int64(7) || args["identifier"] != "wi_imported" || args["request_id"] != "scheduled-priority-"+key {
						t.Fatal("failure edit lost the observed revision or identity")
					}
					if raw, ok := args["labels"]; ok && !reflect.DeepEqual(raw, []string{"operator-label", "ci-infrastructure-failure"}) {
						t.Fatal("infrastructure edit replaced operator labels")
					}
					continue
				}
				body, comment := args["body"].(string)
				if !comment {
					body = args["description"].(string)
					if args["state"] != "Todo" || args["priority"] != 2 {
						t.Fatal("new report did not enter Todo at High")
					}
					if strings.Contains(body, "No source repair is authorized") && !slices.Contains(args["labels"].([]string), "ci-infrastructure-failure") {
						t.Fatal("instance failure has no infrastructure label")
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
					name := scheduledJob(body)
					if origin.Fingerprint != legacyJobFingerprint("scheduled-ci:digitaldrywood/detent:"+name) {
						t.Fatal("native occurrence lost stable job identity")
					}
					if tt.log == diagnostic && tt.laterLog == "" && !strings.Contains(body, "TestOne") && !tt.finalizerFailed {
						t.Fatal("native occurrence lost failing test names")
					}
					if tt.log == verbose.String() {
						for i := range 12 {
							if !strings.Contains(body, fmt.Sprintf("go-test:owner/repo/pkg:TestVerbose%d`", i)) {
								t.Fatal("bounded job evidence lost a failing test name")
							}
						}
					}
					if strings.Contains(tt.name, "multiple findings") && !strings.Contains(body, "TestTwo") {
						t.Fatal("job aggregation lost a failing test")
					}
				}
			}
			if evidence.Len() == 0 && !tt.missingRevision {
				t.Fatal("no retained diagnostic payload")
			}
		})
	}
}

func (f *fakeCloud) ListTools(context.Context) ([]operatortool.Definition, error) {
	var definitions []operatortool.Definition
	for _, name := range []string{"work_config", "work_list", "work_item", "work_comments", "file_issue", "edit_item", "move_item", "add_comment", "connection_info"} {
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
		states                 []tracker.NativeState
		revokedAfterInitialize bool
		priorityEdit           bool
		summaryDetail          bool
		emptyBody              bool
	}{
		{name: "scoped connection and paginated discovery and reads"},
		{name: "priority promotion over scoped connection", priorityEdit: true},
		{name: "read only scope", fail: "file_issue"},
		{name: "missing priority edit authority", fail: "edit_item"},
		{name: "missing transition authority", fail: "move_item"},
		{name: "missing item detail authority", fail: "work_item"},
		{name: "revoked key", status: http.StatusUnauthorized},
		{name: "revoked key on established session", revokedAfterInitialize: true},
		{name: "narrow project grant", fail: "work_config"},
		{name: "provider unavailable", status: http.StatusServiceUnavailable},
		{name: "Backlog must remain nondispatchable", states: []tracker.NativeState{{Name: "Backlog", Dispatchable: true}, {Name: "Todo", Dispatchable: true}}},
		{name: "Backlog must remain nonterminal", states: []tracker.NativeState{{Name: "Backlog", Terminal: true}, {Name: "Todo", Dispatchable: true}}},
		{name: "Backlog must allow reporter creation", states: []tracker.NativeState{{Name: "Backlog", OperatorOnly: true}, {Name: "Todo", Dispatchable: true}}},
		{name: "missing Todo rejects destination", states: []tracker.NativeState{{Name: "Backlog"}}},
		{name: "Todo must be dispatchable", states: []tracker.NativeState{{Name: "Backlog"}, {Name: "Todo"}}},
		{name: "Todo must be nonterminal", states: []tracker.NativeState{{Name: "Backlog"}, {Name: "Todo", Dispatchable: true, Terminal: true}}},
		{name: "Todo must allow automatic admission", states: []tracker.NativeState{{Name: "Backlog"}, {Name: "Todo", Dispatchable: true, OperatorOnly: true}}},
		{name: "malformed list never means no matches", badPage: "work_list"},
		{name: "malformed comments never lose imported matches", badPage: "work_comments"},
		{name: "malformed item detail never loses origin evidence", badPage: "work_item"},
		{name: "body-omitting item detail never loses origin evidence", summaryDetail: true},
		{name: "empty complete item body is valid", emptyBody: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCloud{fail: tt.fail, states: tt.states, badPage: tt.badPage, summaryDetail: tt.summaryDetail}
			f.items = []tracker.NativeIssue{cloudItem("wi_first", 1, "First diagnostic"), cloudItem("wi_second", 2, "Second diagnostic")}
			if tt.emptyBody {
				f.items[0].Body = ""
			}
			f.comments = map[string][]tracker.NativeComment{"wi_second": {
				{OrganizationID: "org", ProjectID: scheduledCloudProject, WorkItemID: "wi_second", Body: "First occurrence"},
				{OrganizationID: "org", ProjectID: scheduledCloudProject, WorkItemID: "wi_second", Body: "Second occurrence"},
			}}
			if tt.badPage == "work_comments" {
				f.items = []tracker.NativeIssue{cloudItem("wi_imported", 1, "Imported work")}
			}
			const fingerprint = "source-fingerprint"
			if tt.priorityEdit {
				body := issueorigin.Stamp("Imported source diagnostic", issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "previous-run", Fingerprint: fingerprint})
				f.items = []tracker.NativeIssue{cloudItem("wi_imported", 1, body)}
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
			transport := &cloudMCP{endpoint: "https://cloud.detent.build/api/v2/organizations/org/mcp", token: "private-test-key", client: client}
			err := transport.initialize(t.Context())
			revoked = tt.revokedAfterInitialize
			destination := &cloudDestination{command: transport.call, project: scheduledCloudProject}
			if err == nil {
				err = destination.load(t.Context())
				if err == nil && tt.priorityEdit {
					body := issueorigin.Stamp("New source occurrence", issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "current-run", Fingerprint: fingerprint})
					err = destination.file(t.Context(), fingerprint, "source failure", body, "scoped-occurrence", nil, true)
				}
			}
			if (err != nil) != (tt.fail != "" || tt.status != 0 || tt.states != nil || tt.badPage != "" || tt.revokedAfterInitialize || tt.summaryDetail) {
				t.Fatalf("transport result %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private-test-key") {
				t.Fatal("secret leaked into diagnostic")
			}
			if err == nil && !tt.priorityEdit {
				if destination.organization != "org" || len(destination.issues) != 2 || destination.issues[1].item.WorkItemID != "wi_second" || !reflect.DeepEqual(destination.issues[1].bodies, []string{"Second diagnostic", "First occurrence", "Second occurrence"}) {
					t.Fatalf("paginated native reads lost destination or occurrence evidence: %+v", destination.issues)
				}
			}
			if tt.priorityEdit && (len(f.writes) != 3 || f.items[0].State != "Todo" || f.items[0].Priority == nil || *f.items[0].Priority != 1 || f.items[0].Revision != 9) {
				t.Fatal("scoped priority edit lost its native owner or occurrence")
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
		{"missing key", "digitaldrywood/detent", "https://cloud.detent.build/api/v2/organizations/org/mcp", scheduledCloudProject, "", false},
		{"wrong project", "digitaldrywood/detent", "https://cloud.detent.build/api/v2/organizations/org/mcp", "prj_other", "private-key", false},
		{"insecure connection", "digitaldrywood/detent", "http://cloud.detent.build/api/v2/organizations/org/mcp", scheduledCloudProject, "private-key", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string {
				return map[string]string{"GITHUB_REPOSITORY": tt.repo, "DETENT_MCP_URL": tt.endpoint, "DETENT_PROJECT_ID": tt.project, "DETENT_API_KEY": tt.token}[key]
			}
			destination, err := reportingDestination(t.Context(), getenv)
			if tt.github {
				github, ok := destination.(*githubDestination)
				if err != nil || !ok {
					t.Fatalf("GitHub mode = %v %v", destination, err)
				}
				f := &fakeGH{}
				github.command = f.command
				if err := github.load(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := github.file(t.Context(), "fingerprint", "source failure", "Other repository source repair", "occurrence", []string{"detent:todo", "hotfix", "ci-scheduled-failure"}, true); err != nil {
					t.Fatal(err)
				}
				if len(f.created) != 1 {
					t.Fatal("other repository lost its existing reporting policy")
				}
				return
			}
			if err == nil || destination != nil {
				t.Fatal("missing authority fell back to a second tracker")
			}
		})
	}
}
