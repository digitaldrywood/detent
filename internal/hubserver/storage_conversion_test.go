package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
)

func TestStorageConversionHistoricalJSONScope(t *testing.T) {
	service := &Service{}
	scope := nativeScope{organization: "org_example", project: "prj_example"}
	raw := []byte(`{"output":"` + strings.Repeat("old snapshot", 2048) + `","runtime":{"landing":{"waiting":true}},"usage":{"output":42}}`)
	ctx := context.WithValue(t.Context(), changeContentKey{}, &changeContent{
		store: &memoryRunDetails{},
		register: func(_ context.Context, organization string, ref attachment.ArchiveReference, _ bool) error {
			return ref.Validate(organization)
		},
	})
	converted, _, err := service.prepareStorageConversion(ctx, scope, storageConversionFamilies[1], 1, raw)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(converted, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope == nil {
		t.Fatal("conversion returned a null envelope")
	}
	envelope["runtime"] = json.RawMessage(`{"landing":{"waiting":false,"landed":true}}`)
	updated, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveStorageJSON(ctx, updated)
	if err != nil {
		t.Fatal(err)
	}
	var run struct {
		Output  string `json:"output"`
		Runtime struct {
			Landing struct {
				Waiting bool `json:"waiting"`
				Landed  bool `json:"landed"`
			} `json:"landing"`
		} `json:"runtime"`
		Usage struct {
			Output int64 `json:"output"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resolved, &run); err != nil {
		t.Fatal(err)
	}
	if run.Output != strings.Repeat("old snapshot", 2048) || run.Runtime.Landing.Waiting || !run.Runtime.Landing.Landed || run.Usage.Output != 42 {
		t.Fatal("mutable landing metadata or immutable historical content changed")
	}
	var ref storageJSONReference
	if err := json.Unmarshal(envelope["_storage_archive"], &ref); err != nil {
		t.Fatal(err)
	}
	ref.Project = "other-project"
	envelope["_storage_archive"], err = json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveStorageJSON(ctx, tampered); err == nil {
		t.Fatal("accepted content from another project")
	}
}

func storageSchemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRowContext(t.Context(), "PRAGMA schema_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}
