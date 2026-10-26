package hubserver

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPreparedConfigurationPreservesOtherProjects(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-configuration")
	first := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), ObservedAt: f.service.config.now()}
	second := runnerauth.ProjectConfiguration{ProjectID: string(other.project.ID), ObservedAt: first.ObservedAt}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	scope.credential.Runner.RunnerID = r.binding.RunnerID
	store := func(view runnerauth.ProjectConfiguration) {
		t.Helper()
		current := scope
		current.project = tracker.ProjectID(view.ProjectID)
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := storeProjectConfigurationObservation(t.Context(), tx, current, view); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	store(first)
	store(second)
	first.Paused = true
	prepared, err := encodeProjectConfiguration(first)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = prepareLegacyProjectConfiguration(t.Context(), f.service.database.reader, scope, prepared)
	if err != nil || !prepared.document.Valid {
		t.Fatalf("configuration map was not prepared: %v", err)
	}
	second.Draining = true
	store(second)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := storePreparedProjectConfiguration(t.Context(), tx, scope, prepared); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []runnerauth.ProjectConfiguration{first, second} {
		current := scope
		current.project = tracker.ProjectID(want.ProjectID)
		raw, err := readProjectConfigurationEntry(t.Context(), f.service.database.reader, current, r.binding.RunnerID)
		if err != nil {
			t.Fatal(err)
		}
		var got runnerauth.ProjectConfiguration
		if err := json.Unmarshal([]byte(raw.String), &got); err != nil {
			t.Fatal(err)
		}
		if got.ProjectID != want.ProjectID || got.Paused != want.Paused || got.Draining != want.Draining || !got.ObservedAt.Equal(want.ObservedAt) {
			t.Fatalf("concurrent project configuration lost: got=%+v want=%+v", got, want)
		}
	}
}

func TestProjectConfigurationMapReplacement(t *testing.T) {
	for _, test := range []struct {
		name, document, project, want string
	}{
		{"replace first", `{"first":{"marker":"old"},"second":{"marker":"keep"}}`, "first", `{"first":{"marker":"new"},"second":{"marker":"keep"}}`},
		{"replace last", `{"first":{"marker":"keep"},"second":{"marker":"old"}}`, "second", `{"first":{"marker":"keep"},"second":{"marker":"new"}}`},
		{"escaped key", `{"first\"key":{"marker":"old"},"second":{"marker":"keep"}}`, `first"key`, `{"first\"key":{"marker":"new"},"second":{"marker":"keep"}}`},
		{"append", `{"first":{"marker":"keep"}}`, "second", `{"first":{"marker":"keep"},"second":{"marker":"new"}}`},
		{"empty", ` { } `, "first", `{"first":{"marker":"new"}}`},
		{"null", `null`, "first", `{"first":{"marker":"new"}}`},
		{"duplicate key", `{"first":{"marker":"old"},"first":{"marker":"last"}}`, "first", `{"first":{"marker":"new"}}`},
		{"array", `[]`, "first", ""},
		{"invalid sibling", `{"first":{},"second":!}`, "first", ""},
		{"invalid suffix", `{} {}`, "first", ""},
		{"null suffix", `null {}`, "first", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := replaceProjectConfigurationEntry(test.document, test.project, `{"marker":"new"}`)
			if test.want == "" {
				if err == nil {
					t.Fatalf("accepted malformed map: %s", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var actual, expected map[string]map[string]string
			if err := json.Unmarshal([]byte(got), &actual); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(test.want), &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("configuration map=%s want=%s", got, test.want)
			}
		})
	}
}
