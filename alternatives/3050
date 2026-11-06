package hubserver

import (
	"context"
	"errors"
	"testing"
)

func TestRetiredOutboxCannotAccessLegacyDatabase(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(*Service) error
	}{
		{"health", func(s *Service) error { _, err := s.OutboxHealth(context.Background()); return err }},
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
