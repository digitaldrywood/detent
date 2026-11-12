package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestFromFeatureError(t *testing.T) {
	t.Parallel()
	plain := errors.New("plain failure")
	detailed := &hubapi.Error{Code: "limit_reached", Message: "Limit reached", CurrentRevision: 9, Details: map[string]any{"limit": 3}, Status: http.StatusTooManyRequests}
	for _, test := range []struct {
		name string
		err  error
		want *nativeError
	}{
		{"invalid", hubapi.Invalid("title is required"), nativeInvalid("title is required").(*nativeError)},
		{"public invalid", hubapi.PublicInvalid("title is required"), nativePublicInvalid("title is required").(*nativeError)},
		{"conflict", hubapi.Conflict(4), nativeConflict(4).(*nativeError)},
		{"wrapped conflict", fmt.Errorf("update thread: %w", hubapi.Conflict(5)), nativeConflict(5).(*nativeError)},
		{"details and revision", detailed, &nativeError{Code: "limit_reached", Message: "Limit reached", CurrentRevision: 9, Details: map[string]any{"limit": 3}, status: http.StatusTooManyRequests}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var got *nativeError
			if !errors.As(fromFeatureError(test.err), &got) {
				t.Fatalf("fromFeatureError(%v) is not a nativeError", test.err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("mapped = %+v, want %+v", got, test.want)
			}
		})
	}
	var notFound *nativeError
	if !errors.As(fromFeatureError(hubapi.NotFound()), &notFound) || !isNativeNotFound(notFound) || notFound.status != http.StatusNotFound || !notFound.publicMessage || len(notFound.logAttributes) != 2 || notFound.logAttributes[0] != "not_found_source" {
		t.Fatalf("mapped not found = %+v", notFound)
	}
	if got := fromFeatureError(plain); got != plain {
		t.Fatalf("plain error mapped to %v", got)
	}
	if got := fromFeatureError(nil); got != nil {
		t.Fatalf("nil mapped to %v", got)
	}
}

type featureScopeReader struct {
	accessstore.Reader
	authority accessstore.Authority
	now       time.Time
	err       error
}

func (r *featureScopeReader) AuthorizeAccess(_ context.Context, authority accessstore.Authority, now time.Time) (accessstore.Member, error) {
	r.authority, r.now = authority, now
	return accessstore.Member{}, r.err
}

func TestFeatureScopeAuthorizesThroughTheService(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	service := &Service{config: Config{now: func() time.Time { return now }}}
	native := nativeScope{organization: "org_example", project: "prj_example", action: "example_action", requireHostedAdmin: true, credential: apiCredential{ID: "credential_example", Hash: "hash_example", HostedRole: "admin", Scope: apiScopeOperator}}
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
			scope := service.featureScope(native)
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
	service := &Service{config: Config{now: func() time.Time { return now }}}
	native := nativeScope{organization: "org_example", project: "prj_example", action: "example_action", credential: apiCredential{ID: "credential_example", Hash: "hash_example", Scope: apiScopeOperator}}
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
			original := service.featureScope(native)
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
