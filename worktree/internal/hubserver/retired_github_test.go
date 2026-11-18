package hubserver

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
)

func TestGitHubWorkersRequirePostgresOwner(t *testing.T) {
	t.Parallel()
	configured := &Service{config: Config{ImportBackend: gitHubLifecycleImporter{}, ReconcileBackend: &gitHubRepairFixture{}}}
	for _, test := range []struct {
		name         string
		call         func(*Service) error
		unconfigured error
	}{
		{"batch maintenance", func(s *Service) error { return s.maintainGitHubBatches(context.Background()) }, nil},
		{"reconciliation", func(s *Service) error { return s.maintainGitHubReconciliationRecords(context.Background()) }, nil},
		{"completion", func(s *Service) error {
			return s.completeRunnerGitHubBatch(context.Background(), hubscope.NativeScope{})
		}, hubapi.ErrPostgresFeatureUnavailable},
		{"advance", func(s *Service) error { return s.advanceGitHubBatch(context.Background(), hubscope.NativeScope{}) }, hubapi.ErrPostgresFeatureUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(configured); !errors.Is(err, hubapi.ErrPostgresFeatureUnavailable) {
				t.Fatalf("configured worker without owner=%v", err)
			}
			if err := test.call(&Service{}); !errors.Is(err, test.unconfigured) {
				t.Fatalf("unconfigured worker=%v want=%v", err, test.unconfigured)
			}
		})
	}
}
