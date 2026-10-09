package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func restFixture(t *testing.T, f *fakeCloud, getenv func(string) string) *cloudDestination {
	t.Helper()
	client := &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mcp") {
			w.WriteHeader(http.StatusBadGateway)
			t.Error("reporter called unavailable MCP")
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-rest-key" {
			t.Fatal("missing scoped REST bearer")
		}
		base := "/api/v2/organizations/org/projects/" + scheduledCloudProject
		if !strings.HasPrefix(r.URL.Path, base) {
			t.Fatalf("foreign REST path %s", r.URL.Path)
		}
		path := strings.TrimPrefix(r.URL.Path, base)
		parts := strings.Split(strings.Trim(path, "/"), "/")
		args := map[string]any{"project_id": scheduledCloudProject}
		var name string
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if r.Method == http.MethodGet {
			if path == "/work-items" && r.URL.Query().Get("include") != "summary" {
				t.Fatal("REST work list lost its bounded summary projection")
			}
			for key, values := range r.URL.Query() {
				if key == "q" {
					key = "query"
				}
				args[key] = values[0]
			}
			if args["open"] == "true" {
				args["open"] = true
			}
			switch len(parts) {
			case 1:
				name = "work_list"
				if path == "" {
					name = "work_config"
				}
			case 2:
				name, args["reference"] = "work_item", parts[1]
			case 3:
				name, args["reference"] = "work_comments", parts[1]
			}
		} else {
			switch {
			case r.Method == http.MethodPost && path == "/work-items":
				var input tracker.CreateIssue
				if err := decoder.Decode(&input); err != nil {
					t.Fatal(err)
				}
				if input.Priority == nil || *input.Priority != 1 {
					t.Fatal("REST creation did not request High")
				}
				name = "file_issue"
				args["request_id"], args["title"], args["description"], args["state"], args["priority"], args["labels"] = input.IdempotencyKey, input.Title, input.Body, input.State, *input.Priority+1, input.Labels
			case r.Method == http.MethodPatch:
				var input tracker.UpdateIssue
				if err := decoder.Decode(&input); err != nil {
					t.Fatal(err)
				}
				name = "edit_item"
				args["request_id"], args["identifier"], args["expected_revision"] = input.IdempotencyKey, parts[1], fmt.Sprint(input.ExpectedRevision)
				if priority := input.Priority.Level(); priority != nil {
					args["priority"] = *priority
				}
				if input.Labels != nil {
					args["labels"] = *input.Labels
				}
			case strings.HasSuffix(path, "/workflow"):
				var input tracker.Transition
				if err := decoder.Decode(&input); err != nil || input.Reason != "worker_progress" {
					t.Fatalf("invalid native transition: %v %+v", err, input)
				}
				name = "move_item"
				args["request_id"], args["identifier"], args["expected_revision"], args["target_state"] = input.IdempotencyKey, parts[1], fmt.Sprint(input.ExpectedRevision), input.State
			case strings.HasSuffix(path, "/comments"):
				var input tracker.CreateComment
				if err := decoder.Decode(&input); err != nil {
					t.Fatal(err)
				}
				name = "add_comment"
				args["request_id"], args["identifier"], args["body"] = input.IdempotencyKey, parts[1], input.Body
			default:
				t.Fatalf("unexpected REST mutation %s %s", r.Method, path)
			}
		}
		var raw json.RawMessage
		if err := f.command(r.Context(), name, args, &raw); err != nil {
			t.Fatal(err)
		}
		var response any
		if r.Method == http.MethodGet {
			var envelope struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			response = envelope.Data
			if name == "work_config" {
				var data struct {
					Project tracker.NativeProject `json:"project"`
				}
				if err := json.Unmarshal(envelope.Data, &data); err != nil {
					t.Fatal(err)
				}
				response = data.Project
			}
		} else if name == "add_comment" {
			comments := f.comments[parts[1]]
			response = comments[len(comments)-1]
		} else {
			id := stringArgument(args, "identifier")
			if name == "file_issue" {
				id = string(f.items[len(f.items)-1].WorkItemID)
			}
			for _, item := range f.items {
				if string(item.WorkItemID) == id {
					response = item
				}
			}
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatal(err)
		}
	})}}
	destination, err := cloudReportingDestination(func(key string) string {
		switch key {
		case "DETENT_MCP_URL":
			return "https://cloud.detent.build/organizations/org/mcp"
		case "DETENT_API_KEY":
			return "test-rest-key"
		case "DETENT_PROJECT_ID":
			return scheduledCloudProject
		default:
			return getenv(key)
		}
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	destination.evidence = io.Discard
	return destination
}

func TestRESTReporting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		green bool
	}{
		{name: "red scheduled suite"},
		{name: "green scheduled reconciliation", green: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := issueorigin.Stamp("Scheduled validation job **Coverage** (failure) failed on development commit old.", issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/digitaldrywood/detent/actions/runs/1", Fingerprint: legacyJobFingerprint("scheduled-ci:digitaldrywood/detent:Coverage")})
			item := cloudItem("wi_old", 1, body)
			item.Labels = []string{"ci-scheduled-failure"}
			f := &fakeCloud{items: []tracker.NativeIssue{item}}
			gh := &fakeGH{logs: map[int64]string{1: "internal/file.go:3:2: nil dereference"}}
			attempt := 1
			getenv := func(key string) string {
				if key == "GITHUB_RUN_ATTEMPT" {
					return strconv.Itoa(attempt)
				}
				return scheduledEnv(key)
			}
			input := `[{"id":1,"name":"Coverage","conclusion":"failure"}]`
			if test.green {
				input = `[{"id":1,"name":"Coverage","conclusion":"success"}]`
			}
			for range 2 {
				if err := reportTo(t.Context(), strings.NewReader(input), gh.command, getenv, restFixture(t, f, getenv)); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.items) != 1 || len(f.comments["wi_old"]) != 1 {
				t.Fatal("REST reporter duplicated an existing occurrence")
			}
			if test.green {
				comment := f.comments["wi_old"][0].Body
				if !strings.Contains(comment, scheduledEnv("CI_DEVELOP_SHA")) || !strings.Contains(comment, "does not establish") || f.items[0].State != "Backlog" || len(gh.created) != 0 || len(gh.comments) != 0 {
					t.Fatalf("green reconciliation changed authority: %s %+v", comment, f.items)
				}
			} else if f.items[0].State != "Todo" || f.items[0].Priority == nil || *f.items[0].Priority != 1 {
				t.Fatal("scheduled REST failure lost Todo/High policy")
			}
			attempt++
			if err := reportTo(t.Context(), strings.NewReader(input), gh.command, getenv, restFixture(t, f, getenv)); err != nil || len(f.comments["wi_old"]) != 2 {
				t.Fatalf("later REST occurrence = %v, comments=%v", err, f.comments)
			}
		})
	}
}

func TestRESTRetries(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		statuses []int
		wantErr  bool
		elapsed  time.Duration
	}{
		{name: "transient gateway failure", statuses: []int{502, 503, 200}, elapsed: 4 * time.Second},
		{name: "persistent outage", statuses: []int{502}, wantErr: true, elapsed: 2 * time.Minute},
		{name: "denied", statuses: []int{403}, wantErr: true},
		{name: "revision conflict", statuses: []int{409}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var bodies [][]byte
				client := &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					index := min(len(bodies), len(test.statuses)-1)
					bodies = append(bodies, body)
					w.WriteHeader(test.statuses[index])
					fmt.Fprint(w, `{"private":"test-rest-key"}`)
				})}}
				transport, err := newCloudREST("https://cloud.detent.build/api/v2/organizations/org/mcp", scheduledCloudProject, "test-rest-key", client)
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				var result json.RawMessage
				err = transport.request(t.Context(), http.MethodPost, "/work-items", map[string]any{"idempotency_key": "stable-occurrence", "body": "failure"}, &result)
				if (err != nil) != test.wantErr || time.Since(start) != test.elapsed {
					t.Fatalf("retry = %v after %s", err, time.Since(start))
				}
				if test.elapsed == 2*time.Minute && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("persistent REST outage lost bounded deadline")
				}
				if err != nil && strings.Contains(err.Error(), "test-rest-key") {
					t.Fatal("REST diagnostic exposed response data")
				}
				for _, body := range bodies {
					if !bytes.Equal(body, bodies[0]) || !strings.Contains(string(body), "stable-occurrence") {
						t.Fatal("retry changed the idempotent publication")
					}
				}
				if test.elapsed == 0 && len(bodies) != 1 || !test.wantErr && len(bodies) != len(test.statuses) {
					t.Fatalf("wrong retry count: %d", len(bodies))
				}
				if !test.wantErr && !reflect.DeepEqual([]byte(result), []byte(`{"private":"test-rest-key"}`)) {
					t.Fatal("successful REST response was lost")
				}
			})
		})
	}
}
