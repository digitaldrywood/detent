package visualreview

import (
	"encoding/json"
	"strings"
	"testing"
)

func prototypeManifest(t *testing.T) []byte {
	t.Helper()
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	m := map[string]any{
		"schema_version": 1, "capture_id": "round-1", "repository": "acme/app", "pr": 42,
		"head_sha": head, "base_sha": base, "captured_at": "2026-09-04T12:00:00Z",
		"title": "Settings review", "summary": "Refresh settings", "coverage_notes": "Desktop and mobile",
		"changed_files": []any{"settings.go", "settings.templ"},
		"assets": []any{
			map[string]any{"id": "full", "path": "media/full.png", "kind": "after", "label": "Settings", "observed": "Page rendered", "inspected": true, "width": 1280, "height": 800, "sha256": strings.Repeat("c", 64), "source": source(head)},
			map[string]any{"id": "detail", "path": "media/detail.png", "kind": "detail", "label": "Save button", "observed": "Button aligned", "inspected": true, "width": 300, "height": 100, "parent_id": "full", "crop": map[string]any{"x": 10, "y": 20, "width": 300, "height": 100}, "source": source(head)},
		},
		"changes": []any{
			map[string]any{"id": "settings", "title": "Settings", "description": "Update layout", "files": []any{"settings.templ"}, "status": "captured", "asset_ids": []any{"full", "detail"}},
			map[string]any{"id": "handler", "title": "Handler", "description": "Support rendering", "files": []any{"settings.go"}, "status": "not-ui", "reason": "Server support only", "asset_ids": []any{}},
		},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func source(commit string) map[string]any {
	return map[string]any{"commit": commit, "url": "http://127.0.0.1:8080/settings", "provenance": "local committed source", "state": "default", "role": "admin", "theme": "light", "conditions": "fixed fixture", "viewport": map[string]any{"width": 1280, "height": 800}}
}

func mutateJSON(t *testing.T, raw []byte, mutate func(map[string]any)) []byte {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	mutate(v)
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestValidateManifest(t *testing.T) {
	t.Parallel()
	raw := prototypeManifest(t)
	m, err := ValidateManifest(raw)
	if err != nil {
		t.Fatalf("valid prototype manifest: %v", err)
	}
	if m.CaptureID != "round-1" || len(m.Assets) != 2 || m.Assets[1].Crop == nil || len(m.Changes) != 2 || !json.Valid(m.Raw) {
		t.Fatalf("unexpected DTO: %#v", m)
	}

	tests := []struct {
		name string
		raw  func(*testing.T) []byte
	}{
		{"after provenance does not match head", func(t *testing.T) []byte {
			return mutateJSON(t, raw, func(v map[string]any) {
				v["assets"].([]any)[0].(map[string]any)["source"].(map[string]any)["commit"] = strings.Repeat("d", 40)
			})
		}},
		{"unknown change asset", func(t *testing.T) []byte {
			return mutateJSON(t, raw, func(v map[string]any) { v["changes"].([]any)[0].(map[string]any)["asset_ids"] = []any{"missing"} })
		}},
		{"changed file lacks coverage", func(t *testing.T) []byte {
			return mutateJSON(t, raw, func(v map[string]any) { v["changes"] = v["changes"].([]any)[:1] })
		}},
		{"null required inspected", func(t *testing.T) []byte {
			return mutateJSON(t, raw, func(v map[string]any) { v["assets"].([]any)[0].(map[string]any)["inspected"] = nil })
		}},
		{"null required arrays", func(t *testing.T) []byte {
			return mutateJSON(t, raw, func(v map[string]any) { v["changed_files"] = nil })
		}},
		{"crop outside parent", func(t *testing.T) []byte {
			return mutateJSON(t, raw, func(v map[string]any) { v["assets"].([]any)[1].(map[string]any)["crop"].(map[string]any)["x"] = 1200 })
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ValidateManifest(tt.raw(t)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func validFeedback(t *testing.T, m Manifest) []byte {
	t.Helper()
	f := map[string]any{"schema_version": 1, "repository": m.Repository, "pr": m.PR, "capture_id": m.CaptureID, "head_sha": m.HeadSHA, "authenticated": false, "author": "Reviewer", "exported_at": "2026-09-04T13:00:00Z", "recommendation": "draft", "asset_approvals": []any{}, "drafts": map[string]any{"settings": "Needs closer review"}, "annotations": []any{
		map[string]any{"id": "pin", "change_id": "settings", "asset_id": "full", "kind": "pin", "text": "Move this", "author": "Reviewer", "created_at": "2026-09-04T13:00:00Z", "x": .2, "y": .3, "color": "#E25a30", "stroke_width": 4},
		map[string]any{"id": "rect", "change_id": "settings", "asset_id": "full", "kind": "rectangle", "text": "Align", "created_at": "2026-09-04T13:00:00Z", "x": .2, "y": .3, "width": .4, "height": .2},
		map[string]any{"id": "arrow", "change_id": "settings", "asset_id": "full", "kind": "arrow", "text": "Flow", "created_at": "2026-09-04T13:00:00Z", "x": .2, "y": .3, "end_x": .8, "end_y": .7},
		map[string]any{"id": "pen", "change_id": "settings", "asset_id": "full", "kind": "pen", "text": "Trace", "created_at": "2026-09-04T13:00:00Z", "x": .2, "y": .3, "points": []any{map[string]any{"x": .2, "y": .3}, map[string]any{"x": .4, "y": .5}}},
	}}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestValidateFeedback(t *testing.T) {
	t.Parallel()
	manifest, err := ValidateManifest(prototypeManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	capture := Capture{Repository: manifest.Repository, PR: manifest.PR, CaptureID: manifest.CaptureID, HeadSHA: manifest.HeadSHA, ManifestJSON: manifest.Raw}
	valid := validFeedback(t, manifest)
	if err := ValidateFeedback(valid, capture); err != nil {
		t.Fatalf("valid feedback: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong capture ref", func(v map[string]any) { v["capture_id"] = "round-2" }},
		{"authenticated omitted", func(v map[string]any) { delete(v, "authenticated") }},
		{"wrong asset ref", func(v map[string]any) {
			v["annotations"].([]any)[0].(map[string]any)["asset_id"] = "detail"
			v["annotations"].([]any)[0].(map[string]any)["change_id"] = "handler"
		}},
		{"coordinate outside bounds", func(v map[string]any) { v["annotations"].([]any)[0].(map[string]any)["x"] = 1.1 }},
		{"null coordinate", func(v map[string]any) { v["annotations"].([]any)[0].(map[string]any)["x"] = nil }},
		{"invalid draft change", func(v map[string]any) { v["drafts"] = map[string]any{"missing": "text"} }},
		{"duplicate asset approvals", func(v map[string]any) {
			a := map[string]any{"asset_id": "full", "author": "Reviewer", "approved_at": "2026-09-04T13:00:00Z"}
			v["asset_approvals"] = []any{a, a}
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw := mutateJSON(t, valid, tt.mutate)
			if err := ValidateFeedback(raw, capture); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidateFeedbackRejectsBlockedApprovalAndNonfiniteJSON(t *testing.T) {
	t.Parallel()
	blocked := mutateJSON(t, prototypeManifest(t), func(v map[string]any) {
		c := v["changes"].([]any)[0].(map[string]any)
		c["status"] = "blocked"
		c["reason"] = "Runtime unavailable"
	})
	m, err := ValidateManifest(blocked)
	if err != nil {
		t.Fatal(err)
	}
	capture := Capture{Repository: m.Repository, PR: m.PR, CaptureID: m.CaptureID, HeadSHA: m.HeadSHA, ManifestJSON: m.Raw}
	feedback := mutateJSON(t, validFeedback(t, m), func(v map[string]any) {
		v["recommendation"] = "recommend-approval"
		v["asset_approvals"] = []any{map[string]any{"asset_id": "full", "author": "R", "approved_at": "2026-09-04T13:00:00Z"}, map[string]any{"asset_id": "detail", "author": "R", "approved_at": "2026-09-04T13:00:00Z"}}
	})
	if err := ValidateFeedback(feedback, capture); err == nil {
		t.Fatal("blocked coverage recommended approval")
	}
	nonfinite := strings.Replace(string(validFeedback(t, m)), `"x":0.2`, `"x":NaN`, 1)
	if err := ValidateFeedback([]byte(nonfinite), capture); err == nil {
		t.Fatal("nonfinite coordinate accepted")
	}
}

func TestValidatorsRejectTrailingAndNullOptionalValues(t *testing.T) {
	t.Parallel()
	manifestRaw := prototypeManifest(t)
	for _, suffix := range []string{` {}`, ` garbage`} {
		if _, err := ValidateManifest(append(append([]byte(nil), manifestRaw...), suffix...)); err == nil {
			t.Fatalf("accepted trailing input %q", suffix)
		}
	}
	tooLargePR := mutateJSON(t, manifestRaw, func(v map[string]any) { v["pr"] = 9007199254740992 })
	if _, err := ValidateManifest(tooLargePR); err == nil {
		t.Fatal("accepted PR beyond JavaScript safe integer")
	}
	nullDigest := mutateJSON(t, manifestRaw, func(v map[string]any) { v["assets"].([]any)[1].(map[string]any)["sha256"] = nil })
	if _, err := ValidateManifest(nullDigest); err == nil {
		t.Fatal("accepted explicit null digest")
	}

	m, err := ValidateManifest(manifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	capture := Capture{Repository: m.Repository, PR: m.PR, CaptureID: m.CaptureID, HeadSHA: m.HeadSHA, ManifestJSON: m.Raw}
	for _, field := range []string{"color", "author", "stroke_width"} {
		feedback := mutateJSON(t, validFeedback(t, m), func(v map[string]any) { v["annotations"].([]any)[0].(map[string]any)[field] = nil })
		if err := ValidateFeedback(feedback, capture); err == nil {
			t.Fatalf("accepted explicit null %s", field)
		}
	}
	nullDrafts := mutateJSON(t, validFeedback(t, m), func(v map[string]any) { v["drafts"] = nil })
	if err := ValidateFeedback(nullDrafts, capture); err == nil {
		t.Fatal("accepted explicit null drafts")
	}
	nullDraft := mutateJSON(t, validFeedback(t, m), func(v map[string]any) {
		v["drafts"] = map[string]any{"settings": nil}
	})
	if err := ValidateFeedback(nullDraft, capture); err == nil {
		t.Fatal("accepted explicit null draft value")
	}
}
