package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/localboard"
)

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
