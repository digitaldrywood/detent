package factory

import (
	"errors"

	"testing"
)

func TestNewFromConfigRejectsNotReadyBackends(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"gitlab", "jira"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			got, err := NewFromConfig(Config{Kind: kind})
			if got != nil {
				t.Fatalf("connector = %T, want nil", got)
			}
			if !errors.Is(err, ErrBackendNotReady) {
				t.Fatalf("error = %v, want ErrBackendNotReady", err)
			}
		})
	}
}

func TestNewFromConfigRejectsUnknownBackend(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"", "github", " GitHub ", "memory", "linear", "local_sqlite", "github_local", "asana"} {
		t.Run(kind, func(t *testing.T) {
			got, err := NewFromConfig(Config{Kind: kind})
			if got != nil {
				t.Fatalf("connector = %T, want nil", got)
			}
			if !errors.Is(err, ErrUnsupportedBackend) {
				t.Fatalf("error = %v, want ErrUnsupportedBackend", err)
			}
		})
	}

}
