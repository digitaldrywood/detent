package neutralimporttest

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func HistoricalV7(t *testing.T, organization tracker.OrganizationID) *Fixture {
	t.Helper()
	fixture := Empty(t, organization)
	columns := map[string]map[string]string{
		"issues": {
			"assignees_json":     "[11,\"assignees_json\",\"TEXT\",1,\"'[]'\",0,0]",
			"author_login":       "[17,\"author_login\",\"TEXT\",1,\"''\",0,0]",
			"body":               "[7,\"body\",\"TEXT\",1,\"''\",0,0]",
			"created_at":         "[15,\"created_at\",\"TEXT\",1,null,0,0]",
			"github_database_id": "[4,\"github_database_id\",\"INTEGER\",0,null,0,0]",
			"github_node_id":     "[3,\"github_node_id\",\"TEXT\",1,null,0,0]",
			"github_number":      "[5,\"github_number\",\"INTEGER\",1,null,0,0]",
			"github_state":       "[9,\"github_state\",\"TEXT\",1,null,0,0]",
			"id":                 "[0,\"id\",\"INTEGER\",0,null,1,0]",
			"labels_json":        "[10,\"labels_json\",\"TEXT\",1,\"'[]'\",0,0]",
			"repository_id":      "[1,\"repository_id\",\"INTEGER\",1,null,0,0]",
			"source_updated_at":  "[13,\"source_updated_at\",\"TEXT\",1,null,0,0]",
			"source_version":     "[12,\"source_version\",\"TEXT\",1,null,0,0]",
			"synchronized_at":    "[14,\"synchronized_at\",\"TEXT\",1,null,0,0]",
			"title":              "[6,\"title\",\"TEXT\",1,null,0,0]",
			"updated_at":         "[16,\"updated_at\",\"TEXT\",1,null,0,0]",
			"url":                "[8,\"url\",\"TEXT\",1,null,0,0]",
			"workflow_state_id":  "[2,\"workflow_state_id\",\"INTEGER\",0,null,0,0]",
		},
		"repositories": {
			"config_json":            "[6,\"config_json\",\"TEXT\",1,\"'{}'\",0,0]",
			"created_at":             "[11,\"created_at\",\"TEXT\",1,null,0,0]",
			"github_database_id":     "[2,\"github_database_id\",\"INTEGER\",0,null,0,0]",
			"github_installation_id": "[5,\"github_installation_id\",\"INTEGER\",0,null,0,0]",
			"github_name":            "[4,\"github_name\",\"TEXT\",1,null,0,0]",
			"github_node_id":         "[1,\"github_node_id\",\"TEXT\",1,null,0,0]",
			"github_owner":           "[3,\"github_owner\",\"TEXT\",1,null,0,0]",
			"id":                     "[0,\"id\",\"INTEGER\",0,null,1,0]",
			"last_reconciled_at":     "[10,\"last_reconciled_at\",\"TEXT\",0,null,0,0]",
			"last_webhook_at":        "[9,\"last_webhook_at\",\"TEXT\",0,null,0,0]",
			"reconcile_cursor":       "[8,\"reconcile_cursor\",\"TEXT\",0,null,0,0]",
			"source_updated_at":      "[14,\"source_updated_at\",\"TEXT\",0,null,0,0]",
			"source_version":         "[13,\"source_version\",\"TEXT\",1,\"''\",0,0]",
			"synchronized_at":        "[15,\"synchronized_at\",\"TEXT\",0,null,0,0]",
			"updated_at":             "[12,\"updated_at\",\"TEXT\",1,null,0,0]",
			"webhook_cursor":         "[7,\"webhook_cursor\",\"TEXT\",0,null,0,0]",
		},
		"workflow_states": {
			"created_at":     "[7,\"created_at\",\"TEXT\",1,null,0,0]",
			"detent_state":   "[4,\"detent_state\",\"TEXT\",1,null,0,0]",
			"dispatchable":   "[6,\"dispatchable\",\"INTEGER\",1,\"0\",0,0]",
			"github_node_id": "[2,\"github_node_id\",\"TEXT\",0,null,0,0]",
			"id":             "[0,\"id\",\"INTEGER\",0,null,1,0]",
			"repository_id":  "[1,\"repository_id\",\"INTEGER\",1,null,0,0]",
			"source_name":    "[3,\"source_name\",\"TEXT\",1,null,0,0]",
			"terminal":       "[5,\"terminal\",\"INTEGER\",1,\"0\",0,0]",
			"updated_at":     "[8,\"updated_at\",\"TEXT\",1,null,0,0]",
		},
		"machines": {
			"capabilities_json": "[3,\"capabilities_json\",\"TEXT\",1,\"'{}'\",0,0]",
			"capacity":          "[4,\"capacity\",\"INTEGER\",1,null,0,0]",
			"display_name":      "[2,\"display_name\",\"TEXT\",1,\"''\",0,0]",
			"hostname":          "[1,\"hostname\",\"TEXT\",1,null,0,0]",
			"id":                "[0,\"id\",\"TEXT\",0,null,1,0]",
			"last_heartbeat_at": "[6,\"last_heartbeat_at\",\"TEXT\",1,null,0,0]",
			"registered_at":     "[7,\"registered_at\",\"TEXT\",1,null,0,0]",
			"updated_at":        "[8,\"updated_at\",\"TEXT\",1,null,0,0]",
			"version":           "[5,\"version\",\"TEXT\",1,null,0,0]",
		},
		"leases": {
			"acquired_at":   "[6,\"acquired_at\",\"TEXT\",1,null,0,0]",
			"created_at":    "[9,\"created_at\",\"TEXT\",1,null,0,0]",
			"expires_at":    "[5,\"expires_at\",\"TEXT\",1,null,0,0]",
			"fencing_token": "[0,\"fencing_token\",\"INTEGER\",0,null,1,0]",
			"issue_id":      "[2,\"issue_id\",\"INTEGER\",1,null,0,0]",
			"lease_id":      "[1,\"lease_id\",\"TEXT\",1,null,0,0]",
			"machine_id":    "[3,\"machine_id\",\"TEXT\",1,null,0,0]",
			"released_at":   "[8,\"released_at\",\"TEXT\",0,null,0,0]",
			"renewed_at":    "[7,\"renewed_at\",\"TEXT\",1,null,0,0]",
			"session_id":    "[4,\"session_id\",\"TEXT\",1,null,0,0]",
			"updated_at":    "[10,\"updated_at\",\"TEXT\",1,null,0,0]",
		},
		"work_events": {
			"fencing_token": "[2,\"fencing_token\",\"INTEGER\",0,null,0,0]",
			"id":            "[0,\"id\",\"INTEGER\",0,null,1,0]",
			"issue_id":      "[1,\"issue_id\",\"INTEGER\",1,null,0,0]",
			"kind":          "[6,\"kind\",\"TEXT\",1,null,0,0]",
			"machine_id":    "[3,\"machine_id\",\"TEXT\",0,null,0,0]",
			"occurred_at":   "[8,\"occurred_at\",\"TEXT\",1,null,0,0]",
			"payload_json":  "[7,\"payload_json\",\"TEXT\",1,\"'{}'\",0,0]",
			"recorded_at":   "[9,\"recorded_at\",\"TEXT\",1,null,0,0]",
			"run_id":        "[5,\"run_id\",\"TEXT\",0,null,0,0]",
			"session_id":    "[4,\"session_id\",\"TEXT\",0,null,0,0]",
		},
		"pull_requests": {
			"base_ref":                     "[12,\"base_ref\",\"TEXT\",1,null,0,0]",
			"base_sha":                     "[13,\"base_sha\",\"TEXT\",1,\"''\",0,0]",
			"checks_summary_json":          "[15,\"checks_summary_json\",\"TEXT\",1,\"'{}'\",0,0]",
			"created_at":                   "[22,\"created_at\",\"TEXT\",1,null,0,0]",
			"draft":                        "[9,\"draft\",\"INTEGER\",1,\"0\",0,0]",
			"github_database_id":           "[4,\"github_database_id\",\"INTEGER\",0,null,0,0]",
			"github_node_id":               "[3,\"github_node_id\",\"TEXT\",1,null,0,0]",
			"github_number":                "[5,\"github_number\",\"INTEGER\",1,null,0,0]",
			"github_state":                 "[8,\"github_state\",\"TEXT\",1,null,0,0]",
			"head_ref":                     "[10,\"head_ref\",\"TEXT\",1,null,0,0]",
			"head_sha":                     "[11,\"head_sha\",\"TEXT\",1,null,0,0]",
			"id":                           "[0,\"id\",\"INTEGER\",0,null,1,0]",
			"issue_id":                     "[2,\"issue_id\",\"INTEGER\",0,null,0,0]",
			"merge_readiness_refreshed_at": "[18,\"merge_readiness_refreshed_at\",\"TEXT\",0,null,0,0]",
			"merge_ready":                  "[17,\"merge_ready\",\"INTEGER\",1,\"0\",0,0]",
			"mergeable_state":              "[14,\"mergeable_state\",\"TEXT\",1,\"''\",0,0]",
			"repository_id":                "[1,\"repository_id\",\"INTEGER\",1,null,0,0]",
			"reviews_summary_json":         "[16,\"reviews_summary_json\",\"TEXT\",1,\"'{}'\",0,0]",
			"source_updated_at":            "[20,\"source_updated_at\",\"TEXT\",1,null,0,0]",
			"source_version":               "[19,\"source_version\",\"TEXT\",1,null,0,0]",
			"synchronized_at":              "[21,\"synchronized_at\",\"TEXT\",1,null,0,0]",
			"title":                        "[6,\"title\",\"TEXT\",1,null,0,0]",
			"updated_at":                   "[23,\"updated_at\",\"TEXT\",1,null,0,0]",
			"url":                          "[7,\"url\",\"TEXT\",1,null,0,0]",
		},
		"queue_entries": {
			"created_at":        "[7,\"created_at\",\"TEXT\",1,null,0,0]",
			"id":                "[0,\"id\",\"INTEGER\",0,null,1,0]",
			"issue_id":          "[1,\"issue_id\",\"INTEGER\",1,null,0,0]",
			"priority_override": "[6,\"priority_override\",\"INTEGER\",0,null,0,0]",
			"rank":              "[5,\"rank\",\"TEXT\",1,null,0,0]",
			"scope":             "[3,\"scope\",\"TEXT\",1,null,0,0]",
			"state":             "[4,\"state\",\"TEXT\",1,null,0,0]",
			"updated_at":        "[8,\"updated_at\",\"TEXT\",1,null,0,0]",
			"workflow_state_id": "[2,\"workflow_state_id\",\"INTEGER\",0,null,0,0]",
		},
		"issue_dependencies": {
			"blocker_issue_id":   "[0,\"blocker_issue_id\",\"INTEGER\",1,null,1,0]",
			"created_at":         "[4,\"created_at\",\"TEXT\",1,null,0,0]",
			"dependent_issue_id": "[1,\"dependent_issue_id\",\"INTEGER\",1,null,2,0]",
			"provenance":         "[2,\"provenance\",\"TEXT\",1,null,0,0]",
			"source_version":     "[3,\"source_version\",\"TEXT\",0,null,0,0]",
			"updated_at":         "[5,\"updated_at\",\"TEXT\",1,null,0,0]",
		},
	}
	keys := map[string][]neutralimport.Key{
		"issues":             {{Column: "id", Kind: "integer"}},
		"repositories":       {{Column: "id", Kind: "integer"}},
		"workflow_states":    {{Column: "id", Kind: "integer"}},
		"machines":           {{Column: "id", Kind: "text"}},
		"leases":             {{Column: "fencing_token", Kind: "integer"}},
		"work_events":        {{Column: "id", Kind: "integer"}},
		"pull_requests":      {{Column: "id", Kind: "integer"}},
		"queue_entries":      {{Column: "id", Kind: "integer"}},
		"issue_dependencies": {{Column: "blocker_issue_id", Kind: "integer"}, {Column: "dependent_issue_id", Kind: "integer"}},
	}
	for name, values := range columns {
		fixture.Tables[name] = neutralimport.Table{Name: name, Columns: values, Key: keys[name]}
	}
	const stamp = "2026-09-01T12:00:00Z"
	fixture.Add("repositories", map[string]any{"id": 1, "github_node_id": "R_original", "github_owner": "example-org", "github_name": "example-project", "created_at": stamp, "updated_at": stamp})
	fixture.Add("workflow_states", map[string]any{"id": 1, "repository_id": 1, "source_name": "Todo", "detent_state": "Todo", "created_at": stamp, "updated_at": stamp})
	for index, id := range []int64{41, 43} {
		suffix := strconv.FormatInt(id, 10)
		fixture.Add("issues", map[string]any{"id": id, "repository_id": 1, "workflow_state_id": 1, "github_node_id": "I_original_" + suffix, "github_number": index + 1, "title": "Imported issue " + suffix, "body": "Original body\r\n" + suffix, "url": "https://github.com/example-org/example-project/issues/" + strconv.Itoa(index+1), "github_state": "open", "source_version": "source-v1", "source_updated_at": stamp, "synchronized_at": stamp, "created_at": stamp, "updated_at": stamp, "author_login": "example-author"})
		machine := "legacy-machine-" + suffix
		fixture.Add("machines", map[string]any{"id": machine, "hostname": "example", "capacity": 1, "version": "example", "last_heartbeat_at": stamp, "registered_at": stamp, "updated_at": stamp})
		fixture.Add("leases", map[string]any{"fencing_token": index + 1, "lease_id": "legacy-lease-" + suffix, "issue_id": id, "machine_id": machine, "session_id": "legacy-session-" + suffix, "expires_at": "2026-09-01T13:00:00Z", "acquired_at": stamp, "renewed_at": stamp, "created_at": stamp, "updated_at": stamp})
		fixture.Add("work_events", map[string]any{"id": index + 7, "issue_id": id, "fencing_token": index + 1, "machine_id": machine, "kind": "legacy-event", "payload_json": `{"reference":"preserved"}`, "occurred_at": stamp, "recorded_at": stamp})
		fixture.Add("pull_requests", map[string]any{"id": index + 1, "repository_id": 1, "issue_id": id, "github_node_id": "PR_original_" + suffix, "github_number": index + 1, "title": "Original PR", "url": "https://github.com/example-org/example-project/pull/" + strconv.Itoa(index+1), "github_state": "open", "head_ref": "change/example", "head_sha": strings.Repeat("a", 40), "base_ref": "develop", "source_version": "source-v1", "source_updated_at": stamp, "synchronized_at": stamp, "created_at": stamp, "updated_at": stamp})
	}
	return fixture
}

func Read(t *testing.T, path string, organization tracker.OrganizationID) *Fixture {
	t.Helper()
	fixture := Empty(t, organization)
	if err := neutralimport.Read(t.Context(), path, organization, func(bundle *neutralimport.Bundle) error {
		for name, columns := range bundle.Schema() {
			keys, err := bundle.TableKey(name)
			if err != nil {
				return err
			}
			fixture.Tables[name] = neutralimport.Table{Name: name, Columns: columns, Key: keys}
			var after []string
			for {
				rows, err := bundle.Page(t.Context(), name, after, 100)
				if err != nil {
					return err
				}
				if len(rows) == 0 {
					break
				}
				fixture.Records[name] = append(fixture.Records[name], rows...)
				after = rows[len(rows)-1].Key
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *Fixture) AddTables(t *testing.T, repository fs.FS, names ...string) {
	t.Helper()
	extra := New(t, repository, f.Organization, names...)
	for name, table := range extra.Tables {
		if _, present := f.Tables[name]; !present {
			f.Tables[name] = table
		}
	}
}
