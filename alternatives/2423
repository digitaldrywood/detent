package pause

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
)

func TestEvaluate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		project    globalconfig.Project
		resolver   connector.IssueReferenceResolver
		repository string
		want       Result
		wantErr    error
		wantFetch  []string
	}{
		{
			name:    "bare pause stays paused",
			project: globalconfig.Project{Paused: true},
		},
		{
			name: "future timestamp stays paused",
			project: globalconfig.Project{
				Paused:      true,
				PausedUntil: now.Add(time.Hour).Format(time.RFC3339),
			},
		},
		{
			name: "expired timestamp lifts pause",
			project: globalconfig.Project{
				Paused:      true,
				PausedUntil: now.Add(-time.Minute).Format(time.RFC3339),
			},
			want: Result{Met: true, Detail: "paused_until reached at 2026-07-27T11:59:00Z"},
		},
		{
			name: "open issue stays paused",
			project: globalconfig.Project{
				Paused:           true,
				PausedUntilIssue: "digitaldrywood/detent#1499",
			},
			resolver:  &pauseResolver{issues: []connector.Issue{{Identifier: "digitaldrywood/detent#1499"}}},
			wantFetch: []string{"digitaldrywood/detent#1499"},
		},
		{
			name: "repository relative issue reference",
			project: globalconfig.Project{
				Paused:           true,
				PausedUntilIssue: "detent#1499",
			},
			repository: "digitaldrywood/detent",
			resolver:   &pauseResolver{issues: []connector.Issue{{Identifier: "digitaldrywood/detent#1499"}}},
			wantFetch:  []string{"digitaldrywood/detent#1499"},
		},
		{
			name: "closed issue lifts pause",
			project: globalconfig.Project{
				Paused:           true,
				PausedUntilIssue: "digitaldrywood/detent#1499",
			},
			resolver:  &pauseResolver{issues: []connector.Issue{{Identifier: "digitaldrywood/detent#1499", Closed: true}}},
			want:      Result{Met: true, Detail: "pause exit issue digitaldrywood/detent#1499 is closed"},
			wantFetch: []string{"digitaldrywood/detent#1499"},
		},
		{
			name: "issue resolver required",
			project: globalconfig.Project{
				Paused:           true,
				PausedUntilIssue: "digitaldrywood/detent#1499",
			},
			wantErr: ErrIssueResolverUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Evaluate(context.Background(), tt.project, now, tt.repository, tt.resolver)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Evaluate() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("Evaluate() = %#v, want %#v", got, tt.want)
			}
			if resolver, ok := tt.resolver.(*pauseResolver); ok && !reflect.DeepEqual(resolver.fetched, tt.wantFetch) {
				t.Fatalf("fetched identifiers = %#v, want %#v", resolver.fetched, tt.wantFetch)
			}
		})
	}
}

func TestHeldLongerThan(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		project globalconfig.Project
		want    bool
	}{
		{
			name: "stale indefinite pause",
			project: globalconfig.Project{
				Paused:   true,
				PausedAt: now.Add(-8 * 24 * time.Hour).Format(time.RFC3339),
			},
			want: true,
		},
		{
			name: "recent indefinite pause",
			project: globalconfig.Project{
				Paused:   true,
				PausedAt: now.Add(-24 * time.Hour).Format(time.RFC3339),
			},
		},
		{
			name: "legacy pause without timestamp",
			project: globalconfig.Project{
				Paused: true,
			},
		},
		{
			name: "exit condition excludes stale warning",
			project: globalconfig.Project{
				Paused:           true,
				PausedAt:         now.Add(-8 * 24 * time.Hour).Format(time.RFC3339),
				PausedUntilIssue: "digitaldrywood/detent#1499",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := HeldLongerThan(tt.project, now, 7*24*time.Hour); got != tt.want {
				t.Fatalf("HeldLongerThan() = %t, want %t", got, tt.want)
			}
		})
	}
}

type pauseResolver struct {
	issues  []connector.Issue
	err     error
	fetched []string
}

func (r *pauseResolver) FetchIssueStatesByIdentifiers(_ context.Context, identifiers []string) ([]connector.Issue, error) {
	r.fetched = append([]string(nil), identifiers...)
	return append([]connector.Issue(nil), r.issues...), r.err
}
