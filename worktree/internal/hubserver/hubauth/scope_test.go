package hubauth_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type featureScopeReader struct {
	accessstore.Reader
	authority accessstore.Authority
	now       time.Time
	err       error
	calls     int
}

func (r *featureScopeReader) AuthorizeAccess(_ context.Context, authority accessstore.Authority, now time.Time) (accessstore.Member, error) {
	if r.calls == 0 {
		r.authority, r.now = authority, now
	}
	r.calls++
	return accessstore.Member{}, r.err
}

func TestFeatureScopeAuthorizesThroughTheService(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	service := hubauth.New(hubauth.Deps{Now: func() time.Time { return now }})
	native := hubauth.Input{Organization: "org_example", Project: "prj_example", Action: "example_action", Admin: true, Credential: hubauth.Credential{ID: "credential_example", Hash: "hash_example", HostedRole: "admin", Scope: hubauth.Operator}}
	for _, test := range []struct {
		name  string
		write bool
		err   error
	}{
		{"read allowed", false, nil},
		{"write allowed", true, nil},
		{"write denied", true, accessstore.ErrDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader := &featureScopeReader{err: test.err}
			scope := service.Scope(native)
			err := scope.Authorize(t.Context(), reader, test.write)
			if !errors.Is(err, test.err) || (test.err == nil) != (err == nil) {
				t.Fatalf("Authorize = %v, want %v", err, test.err)
			}
			want := accessstore.Authority{Action: "example_action", CredentialID: "credential_example", Hash: "hash_example", Role: "admin", ProjectID: "prj_example", Write: test.write, Admin: true}
			if reader.authority != want || !reader.now.Equal(now) {
				t.Fatalf("authority = %+v at %s, want %+v at %s", reader.authority, reader.now, want, now)
			}
			facts := hubapi.Facts{Organization: "org_example", Project: "prj_example", Actor: tracker.Actor{Kind: "human", PrincipalID: "credential_example"}, CredentialID: "credential_example", HostedRole: "admin", Admin: true}
			if !reflect.DeepEqual(scope.Facts, facts) {
				t.Fatalf("facts = %+v, want %+v", scope.Facts, facts)
			}
		})
	}
}

func TestFeatureScopeForProjectAuthorizesTheNamedProject(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	service := hubauth.New(hubauth.Deps{Now: func() time.Time { return now }})
	native := hubauth.Input{Organization: "org_example", Project: "prj_example", Action: "example_action", Credential: hubauth.Credential{ID: "credential_example", Hash: "hash_example", Scope: hubauth.Operator}}
	for _, test := range []struct {
		name    string
		project tracker.ProjectID
		err     error
	}{
		{"other project allowed", "prj_other", nil},
		{"other project denied", "prj_other", accessstore.ErrDenied},
		{"organization level", "", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader := &featureScopeReader{err: test.err}
			original := service.Scope(native)
			rebound := original.ForProject(test.project)
			err := rebound.Authorize(t.Context(), reader, true)
			if !errors.Is(err, test.err) || (test.err == nil) != (err == nil) {
				t.Fatalf("Authorize = %v, want %v", err, test.err)
			}
			want := accessstore.Authority{Action: "example_action", CredentialID: "credential_example", Hash: "hash_example", ProjectID: test.project, Write: true}
			if reader.authority != want {
				t.Fatalf("authority = %+v, want %+v", reader.authority, want)
			}
			if rebound.Project != test.project || original.Project != "prj_example" || rebound.Organization != "org_example" {
				t.Fatalf("rebound = %+v, original = %+v", rebound.Facts, original.Facts)
			}
		})
	}
}
