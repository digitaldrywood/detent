package hubapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestScopeForProject(t *testing.T) {
	t.Parallel()
	facts := hubapi.Facts{Organization: "org_example", Project: "prj_first", CredentialID: "credential_example"}
	granted := map[tracker.ProjectID]bool{"prj_first": true, "prj_second": true}
	type call struct {
		project tracker.ProjectID
		write   bool
	}
	for _, test := range []struct {
		name      string
		rebinding bool
		project   tracker.ProjectID
		write     bool
		want      error
		wantCalls []call
	}{
		{"rebound project allowed", true, "prj_second", true, nil, []call{{"prj_second", true}}},
		{"rebound project denied", true, "prj_ungranted", false, accessstore.ErrDenied, []call{{"prj_ungranted", false}}},
		{"rebound to organization level", true, "", false, accessstore.ErrDenied, []call{{"", false}}},
		{"same project keeps working", true, "prj_first", false, nil, []call{{"prj_first", false}}},
		{"fixed scope refuses another project", false, "prj_second", false, accessstore.ErrDenied, nil},
		{"fixed scope allows its own project", false, "prj_first", true, nil, []call{{"prj_first", true}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls []call
			authorize := func(_ context.Context, _ accessstore.Reader, project tracker.ProjectID, write bool) error {
				calls = append(calls, call{project, write})
				if !granted[project] {
					return accessstore.ErrDenied
				}
				return nil
			}
			original := hubapi.NewProjectScope(facts, authorize)
			if !test.rebinding {
				original = hubapi.NewScope(facts, func(ctx context.Context, reader accessstore.Reader, write bool) error {
					return authorize(ctx, reader, facts.Project, write)
				})
			}
			rebound := original.ForProject(test.project)
			err := rebound.Authorize(t.Context(), authorizeReader{}, test.write)
			if !errors.Is(err, test.want) || (test.want == nil) != (err == nil) {
				t.Fatalf("Authorize = %v, want %v", err, test.want)
			}
			if len(calls) != len(test.wantCalls) {
				t.Fatalf("authorizer calls = %v, want %v", calls, test.wantCalls)
			}
			for index := range calls {
				if calls[index] != test.wantCalls[index] {
					t.Fatalf("authorizer calls = %v, want %v", calls, test.wantCalls)
				}
			}
			if rebound.Project != test.project || rebound.Organization != facts.Organization || rebound.CredentialID != facts.CredentialID {
				t.Fatalf("rebound facts = %+v", rebound.Facts)
			}
			if original.Project != facts.Project {
				t.Fatalf("ForProject changed the original scope to %q", original.Project)
			}
		})
	}
	if err := (hubapi.Scope{}).ForProject("prj_first").Authorize(t.Context(), authorizeReader{}, false); !errors.Is(err, accessstore.ErrDenied) {
		t.Fatalf("zero scope rebound = %v", err)
	}
}

func TestTimeBounds(t *testing.T) {
	t.Parallel()
	early := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	late := early.Add(time.Nanosecond)
	sameInstant := early.In(time.FixedZone("example", 3600))
	for _, test := range []struct {
		name             string
		a, b             time.Time
		wantMin, wantMax time.Time
	}{
		{"ordered", early, late, early, late},
		{"reversed", late, early, early, late},
		{"zero is earliest", time.Time{}, early, time.Time{}, early},
		{"equal instants return the second argument", early, sameInstant, sameInstant, sameInstant},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := hubapi.MinTime(test.a, test.b); got != test.wantMin {
				t.Fatalf("MinTime = %v, want %v", got, test.wantMin)
			}
			if got := hubapi.MaxTime(test.a, test.b); got != test.wantMax {
				t.Fatalf("MaxTime = %v, want %v", got, test.wantMax)
			}
		})
	}
}

func TestStringValue(t *testing.T) {
	t.Parallel()
	value := "example"
	empty := ""
	for _, test := range []struct {
		name  string
		value *string
		want  string
	}{
		{"nil", nil, ""}, {"empty", &empty, ""}, {"set", &value, "example"},
	} {
		if got := hubapi.StringValue(test.value); got != test.want {
			t.Fatalf("%s: StringValue = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestBoundRunes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value string
		limit int
		want  string
	}{
		{"under limit", "example", 10, "example"},
		{"at limit", "example", 7, "example"},
		{"over limit", "example", 3, "exa…"},
		{"counts runes not bytes", "héllo", 5, "héllo"},
		{"cuts on a rune boundary", "héllo", 2, "hé…"},
		{"zero limit", "example", 0, "…"},
		{"empty", "", 0, ""},
	} {
		if got := hubapi.BoundRunes(test.value, test.limit); got != test.want {
			t.Fatalf("%s: BoundRunes = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestConversationTime(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value time.Time
		want  string
	}{
		{"whole second keeps nine fraction digits", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "2026-01-02T03:04:05.000000000Z"},
		{"nanoseconds", time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC), "2026-01-02T03:04:05.123456789Z"},
		{"converted to UTC", time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("example", 2*3600)), "2026-01-02T01:04:05.000000000Z"},
	} {
		if got := hubapi.ConversationTime(test.value); got != test.want {
			t.Fatalf("%s: ConversationTime = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestLesserHostedRole(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ a, b, want string }{
		{"owner", "admin", "admin"}, {"admin", "owner", "admin"},
		{"member", "viewer", "viewer"}, {"reporter", "member", "reporter"},
		{"owner", "owner", "owner"},
		{"viewer", "", "viewer"}, {"", "viewer", ""},
		{"unknown", "owner", "unknown"}, {"owner", "unknown", "unknown"},
		{"", "", ""},
	} {
		if got := hubapi.LesserHostedRole(test.a, test.b); got != test.want {
			t.Fatalf("LesserHostedRole(%q, %q) = %q, want %q", test.a, test.b, got, test.want)
		}
	}
}

func TestStorageUnavailableErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		err     error
		message string
	}{
		{"work item", hubapi.WorkItemStorageUnavailable(), "This work item operation requires storage that is not available"},
		{"run event", hubapi.RunEventStorageUnavailable(), "Run event storage is not available"},
	} {
		var failure *hubapi.Error
		if !errors.As(test.err, &failure) {
			t.Fatalf("%s: not a hubapi.Error: %v", test.name, test.err)
		}
		if failure.Code != "not_ready" || failure.Message != test.message || failure.Status != http.StatusServiceUnavailable || !failure.Public || failure.Error() != test.message || failure.Details != nil || failure.CurrentRevision != 0 || len(failure.LogAttributes()) != 0 {
			t.Fatalf("%s: error = %+v", test.name, failure)
		}
	}
	if hubapi.SearchTermLimit != 32 {
		t.Fatalf("search term limit = %d", hubapi.SearchTermLimit)
	}
}
