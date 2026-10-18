package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/localboard"
)

func TestLocalBoardImportCommand(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		dryRun bool
	}{
		{"preview", true},
		{"publish", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture, err := os.ReadFile(filepath.Join("..", "localboard", "testdata", "local_sqlite.sql"))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "board.sqlite")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.ExecContext(t.Context(), string(fixture)); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var calls []localboard.Request
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v2/organizations/org_example/projects/prj_example/local-board-import" || r.Header.Get("Authorization") != "Bearer scoped-token" {
					t.Errorf("unexpected scoped import request: %s %s", r.Method, r.URL.Path)
				}
				var request localboard.Request
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				mu.Lock()
				calls = append(calls, request)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(localboard.Result{SourceID: request.Record.SourceID, State: request.Record.State, Action: "created"}); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			cmd := NewRootCommand(t.Context(), func(opts *options) {
				opts.lookupEnv = func(name string) string {
					if name == "DETENT_HUB_TOKEN" {
						return "scoped-token"
					}
					return ""
				}
			})
			args := []string{"hub", "issue", "import-local", path, "--hub-url", server.URL, "--organization", "org_example", "--project", "prj_example", "--store-id", "example-board", "--lane-map", "todo=Backlog,Retired lane=Backlog"}
			if test.dryRun {
				args = append(args, "--dry-run")
			}
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			var results []localboard.Result
			if err := json.Unmarshal(output.Bytes(), &results); err != nil || len(results) != 2 || results[0].SourceID != "z-first" || results[1].SourceID != "a-second" {
				t.Fatalf("import results = %s, error = %v", output.String(), err)
			}
			mu.Lock()
			defer mu.Unlock()
			wantCalls := 4
			if test.dryRun {
				wantCalls = 2
			}
			if len(calls) != wantCalls {
				t.Fatalf("import requests = %d, want %d", len(calls), wantCalls)
			}
			for i, call := range calls {
				if call.DryRun != (i < 2) || call.Record.State != "Backlog" || call.Record.Kind != "local_sqlite" {
					t.Fatalf("import request %d lost preflight or lane mapping: %+v", i, call)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("source board changed: %v", err)
			}
		})
	}
}

type localBoardTestImporter struct {
	calls         []localboard.Request
	failPreflight bool
}

func (f *localBoardTestImporter) ImportLocalBoard(_ context.Context, request localboard.Request) (localboard.Result, error) {
	f.calls = append(f.calls, request)
	if f.failPreflight && request.Record.SourceID == "two" {
		return localboard.Result{}, errors.New("title-secret body-secret")
	}
	return localboard.Result{SourceID: request.Record.SourceID, Action: "created", State: request.Record.State}, nil
}

func TestLocalBoardImportPreflight(t *testing.T) {
	t.Parallel()
	records := []localboard.Record{}
	for _, id := range []string{"one", "two"} {
		records = append(records, localboard.Record{Key: localboard.SourceKey("store", "default", "local_sqlite", id), Kind: "local_sqlite", SourceID: id, Author: "author", CreatedAt: time.Unix(0, 0), UpdatedAt: time.Unix(0, 0), State: "Backlog"})
	}
	for _, test := range []struct {
		name         string
		dryRun, fail bool
		wantCalls    []bool
	}{
		{"dry-run", true, false, []bool{true, true}},
		{"real-run", false, false, []bool{true, true, false, false}},
		{"failed preflight publishes nothing", false, true, []bool{true, true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &localBoardTestImporter{failPreflight: test.fail}
			result, err := importLocalBoardRecords(t.Context(), client, records, test.dryRun)
			if (err != nil) != test.fail {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if err != nil && (strings.Contains(err.Error(), "title-secret") || strings.Contains(err.Error(), "body-secret")) {
				t.Fatal("error leaked issue content")
			}
			calls := []bool{}
			for _, call := range client.calls {
				calls = append(calls, call.DryRun)
			}
			if !reflect.DeepEqual(calls, test.wantCalls) {
				t.Fatalf("calls %v want %v", calls, test.wantCalls)
			}
			if !test.fail && len(result) != 2 {
				t.Fatalf("results: %+v", result)
			}
		})
	}
}
