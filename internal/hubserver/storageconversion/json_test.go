package storageconversion_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
)

func TestStorageConversionHistoricalJSONScope(t *testing.T) {
	organization, project := "org_example", "prj_example"
	raw := []byte(`{"output":"` + strings.Repeat("old snapshot", 2048) + `","runtime":{"landing":{"waiting":true}},"usage":{"output":42}}`)
	ctx := t.Context()
	store := hubtest.NewDetails()
	payload, err := json.Marshal(storageconversion.JSONPayload{Organization: organization, Project: project, Family: "attempts", Row: 1, Data: raw})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := store.StoreDetail(ctx, organization, payload)
	if err != nil {
		t.Fatal(err)
	}
	converted, err := storageconversion.CompactJSON(raw, storageconversion.JSONReference{Organization: organization, Project: project, Family: "attempts", Row: 1, Reference: archive})
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
	resolved, err := storageconversion.ResolveJSONWith(ctx, updated, store.ReadDetail)
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
	var ref storageconversion.JSONReference
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
	if _, err := storageconversion.ResolveJSONWith(ctx, tampered, store.ReadDetail); err == nil {
		t.Fatal("accepted content from another project")
	}
}
