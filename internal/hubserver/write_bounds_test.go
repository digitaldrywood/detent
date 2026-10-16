package hubserver

import (
	"database/sql/driver"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestINV18RequestCommitDoesNoDerivedWork(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "bounded write")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	var statements []string
	ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
		statements = append(statements, query)
	}})
	tx, err := f.service.database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := appendNativeHistory(ctx, tx, scope, string(issue.WorkItemID), "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}, f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	statements = statements[:0]
	if err := f.service.database.commit(ctx, tx); err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if strings.Contains(statement, "analytics_") {
			t.Fatalf("request commit ran derived analytics work: %s", statement)
		}
	}
}

func TestINV18HubMigrationsUseTypedColumns(t *testing.T) {
	const adopted = "20261015000000"
	jsonColumn := regexp.MustCompile(`(?i)\b\w*json\w*\s+(TEXT|BLOB)\b|json_valid\s*\(`)
	paths, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if filepath.Base(path) < adopted {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(raw), "-- +goose Down")
		if match := jsonColumn.FindString(up); match != "" {
			t.Errorf("%s adds a JSON column (%q); store typed columns", filepath.Base(path), match)
		}
	}
}
