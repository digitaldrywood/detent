package claim_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/claim"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type hydrateSource struct{ released []tracker.LeaseID }

func (s *hydrateSource) Recovery(context.Context, tracker.NativeWorkItemID) (tracker.NativeRecovery, error) {
	return tracker.NativeRecovery{}, nil
}

func (s *hydrateSource) Release(_ context.Context, lease tracker.NativeLease, _ string) error {
	s.released = append(s.released, lease.ID)
	return nil
}

func (s *hydrateSource) Forget(tracker.NativeLease) {}

func (s *hydrateSource) Intake(context.Context, tracker.NativeLease, tracker.NativeIssue) error {
	return nil
}

func TestHydrateLaterClaimRefusal(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		err      error
		retained int
	}{
		{"later candidate not found keeps won lease", &api.APIError{Status: http.StatusNotFound, Code: "not_found"}, 1},
		{"later candidate contract refusal keeps won lease", &api.APIError{Status: http.StatusConflict, Code: "issue_contract_unsatisfied"}, 1},
		{"later candidate revision conflict keeps won lease", &api.APIError{Status: http.StatusConflict, Code: "revision_conflict"}, 1},
		{"machine not found releases won lease", &api.APIError{Status: http.StatusNotFound, Code: "machine_not_found"}, 0},
		{"unknown conflict releases won lease", &api.APIError{Status: http.StatusConflict, Code: "selector_mismatch"}, 0},
		{"unauthorized releases won lease", &api.APIError{Status: http.StatusUnauthorized, Code: "unauthorized"}, 0},
		{"policy mismatch releases won lease", &api.APIError{Status: http.StatusConflict, Code: "policy_mismatch"}, 0},
		{"runner draining releases won lease", &api.APIError{Status: http.StatusConflict, Code: "runner_draining"}, 0},
		{"lost claim releases won lease", &api.APIError{Status: http.StatusConflict, Code: "stale_fencing_token"}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := &hydrateSource{}
			lease := tracker.NativeLease{ID: "lease-won", WorkItemID: "wi_won", FencingToken: 3}
			result, err := claim.Hydrate(t.Context(), source, []tracker.NativeLease{lease}, false, test.err)
			if len(result.Candidates) != test.retained {
				t.Fatalf("retained=%d want %d error=%v", len(result.Candidates), test.retained, err)
			}
			if test.retained == 1 {
				if err != nil || len(source.released) != 0 || !errors.Is(result.Degraded, test.err) || result.Candidates[0].Lease.ID != lease.ID {
					t.Fatalf("result=%+v released=%v error=%v", result, source.released, err)
				}
				return
			}
			if err == nil || len(source.released) != 1 || source.released[0] != lease.ID {
				t.Fatalf("released=%v error=%v", source.released, err)
			}
		})
	}
}
