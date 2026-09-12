package releaseprovenance

import (
	"strings"
	"testing"
)

const (
	testCommit      = "0123456789abcdef0123456789abcdef01234567"
	testOtherCommit = "89abcdef0123456789abcdef0123456789abcdef"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	valid := Manifest{
		Schema:     Schema,
		Repository: "digitaldrywood/detent",
		Tag:        "v1.2.3",
		Commit:     testCommit,
		Checks:     []Check{{Name: "CI", Status: "completed", Conclusion: "success", CheckRunID: 42}},
	}
	tests := []struct {
		name    string
		mutate  func(*Manifest)
		commit  string
		wantErr string
	}{
		{name: "valid", commit: testCommit},
		{name: "wrong commit", commit: testOtherCommit, wantErr: "want"},
		{name: "short commit", commit: testCommit, mutate: func(m *Manifest) { m.Commit = testCommit[:7] }, wantErr: "not a full git commit"},
		{name: "missing checks", commit: testCommit, mutate: func(m *Manifest) { m.Checks = nil }, wantErr: "no mandatory check evidence"},
		{name: "duplicate checks", commit: testCommit, mutate: func(m *Manifest) { m.Checks = append(m.Checks, m.Checks[0]) }, wantErr: "duplicate"},
		{name: "pending check", commit: testCommit, mutate: func(m *Manifest) { m.Checks[0].Status = "in_progress" }, wantErr: "want completed/success"},
		{name: "cancelled check", commit: testCommit, mutate: func(m *Manifest) { m.Checks[0].Conclusion = "cancelled" }, wantErr: "want completed/success"},
		{name: "skipped check", commit: testCommit, mutate: func(m *Manifest) { m.Checks[0].Conclusion = "skipped" }, wantErr: "want completed/success"},
		{name: "stale check", commit: testCommit, mutate: func(m *Manifest) { m.Checks[0].Conclusion = "stale" }, wantErr: "want completed/success"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			manifest := valid
			manifest.Checks = append([]Check(nil), valid.Checks...)
			if tt.mutate != nil {
				tt.mutate(&manifest)
			}
			err := Validate(manifest, valid.Repository, valid.Tag, tt.commit)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestTagAnnotationRoundTrip(t *testing.T) {
	t.Parallel()

	manifest := Manifest{
		Schema:     Schema,
		Repository: "digitaldrywood/detent",
		Tag:        "v1.2.3",
		Commit:     testCommit,
		Checks:     []Check{{Name: "CI", Status: "completed", Conclusion: "success"}},
	}
	annotation, err := Annotation(manifest)
	if err != nil {
		t.Fatalf("Annotation() error = %v", err)
	}
	got, err := FromTagMessage("release notes\n\n"+annotation+"\n\norigins", manifest.Repository, manifest.Tag, manifest.Commit)
	if err != nil {
		t.Fatalf("FromTagMessage() error = %v", err)
	}
	if got.Commit != manifest.Commit || len(got.Checks) != 1 || got.Checks[0].Name != "CI" {
		t.Fatalf("FromTagMessage() = %#v, want %#v", got, manifest)
	}
}

func TestParseRejectsTamperedOrAmbiguousJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "unknown field", raw: `{"schema":1,"repository":"digitaldrywood/detent","tag":"v1.2.3","commit":"` + testCommit + `","checks":[],"extra":true}`, want: "unknown field"},
		{name: "multiple values", raw: `{}` + "\n{}", want: "multiple JSON values"},
		{name: "missing annotation", raw: "ordinary tag message", want: "missing provenance annotation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var err error
			if tt.name == "missing annotation" {
				_, err = FromTagMessage(tt.raw, "digitaldrywood/detent", "v1.2.3", testCommit)
			} else {
				_, err = Parse([]byte(tt.raw), "digitaldrywood/detent", "v1.2.3", testCommit)
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestFromTagMessageRejectsMultipleAnnotations(t *testing.T) {
	t.Parallel()

	manifest := Manifest{
		Schema:     Schema,
		Repository: "digitaldrywood/detent",
		Tag:        "v1.2.3",
		Commit:     testCommit,
		Checks:     []Check{{Name: "CI", Status: "completed", Conclusion: "success"}},
	}
	annotation, err := Annotation(manifest)
	if err != nil {
		t.Fatalf("Annotation() error = %v", err)
	}
	_, err = FromTagMessage(annotation+"\n"+annotation, manifest.Repository, manifest.Tag, manifest.Commit)
	if err == nil || !strings.Contains(err.Error(), "multiple provenance annotations") {
		t.Fatalf("FromTagMessage() error = %v, want multiple annotations", err)
	}
}
