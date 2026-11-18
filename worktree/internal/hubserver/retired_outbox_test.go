package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestRetiredOutboxHealthSurfaceIsAbsent(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "retired outbox health")
	for _, path := range []string{"/api/v1/outbox/health", "/api/v1/outbox/health?limit=1"} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil), http.StatusNotFound)
	}
	raw, err := json.Marshal(healthResponse{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, found := fields["outbox"]; found {
		t.Fatalf("health still reports the retired outbox: %s", raw)
	}
}

func TestRetiredOutboxCannotAccessLegacyDatabase(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		call func(*Service) error
	}{
		{"claim", func(s *Service) error { _, _, err := s.claimOutbox(context.Background()); return err }},
		{"commit", func(s *Service) error {
			_, err := s.commitOutbox(context.Background(), outboxRecord{}, nil)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(&Service{}); !errors.Is(err, ErrOutboxDisabled) {
				t.Fatalf("error = %v, want disabled", err)
			}
		})
	}
}
