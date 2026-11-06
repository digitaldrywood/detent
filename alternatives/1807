package hubserver

import (
	"context"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestImportPreflightNeedsNoLegacyDatabase(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(t.Context(), contextKey{}, "preserved")
	service := &Service{}
	for _, tt := range []struct {
		name  string
		input any
	}{
		{"run event", tracker.NativeRunEvent{Data: tracker.NativeRunData{AttemptID: "attempt"}}},
		{"batch command", tracker.GitHubBatchCommand{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := service.prepareHostedImportMutation(ctx, nativeScope{}, "POST /imports/example", tt.input)
			if err != nil {
				t.Fatal(err)
			}
			if got.Value(contextKey{}) != "preserved" {
				t.Fatal("import preflight lost request context")
			}
		})
	}
}
