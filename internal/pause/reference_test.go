package pause

import (
	"strings"
	"testing"
)

func TestResolveReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		pausedID  string
		reference string
		trackers  []Tracker
		want      ReferenceResolution
		wantErr   string
	}{
		{
			name:      "native project number without GitHub tracker",
			pausedID:  "prj_example",
			reference: "prj_example#42",
			trackers: []Tracker{{
				ProjectID: "prj_example", Kind: "hub_native", Repository: "example-org/example-project",
			}},
			want: ReferenceResolution{ProjectID: "prj_example", Reference: "prj_example#42"},
		},
		{
			name:      "native project number uses current native tracker",
			pausedID:  "prj_example",
			reference: "prj_example#42",
			trackers: []Tracker{
				{ProjectID: "prj_example", Kind: "hub_native", Repository: "example-org/example-project"},
				{ProjectID: "github-project", Kind: "github", Repository: "example-org/prj_example"},
			},
			want: ReferenceResolution{ProjectID: "prj_example", Reference: "prj_example#42"},
		},
		{
			name:      "native number stays scoped to native tracker",
			pausedID:  "prj_example",
			reference: "#42",
			trackers: []Tracker{{
				ProjectID: "prj_example", Kind: "hub_native", Repository: "example-org/example-project",
			}},
			want: ReferenceResolution{ProjectID: "prj_example", Reference: "#42"},
		},
		{
			name:      "native work item uses current native tracker",
			pausedID:  "prj_example",
			reference: "wi_example",
			trackers: []Tracker{{
				ProjectID: "prj_example", Kind: "hub_native", Repository: "example-org/example-project",
			}},
			want: ReferenceResolution{ProjectID: "prj_example", Reference: "wi_example"},
		},
		{
			name:      "local sqlite rejects unmatched GitHub reference",
			pausedID:  "video",
			reference: "digitaldrywood/video-studio#147",
			trackers: []Tracker{{
				ProjectID: "video",
				Kind:      "local_sqlite",
			}},
			wantErr: "no configured project tracker can resolve GitHub pause exit issue",
		},
		{
			name:      "GitHub rejects local sqlite reference",
			pausedID:  "detent",
			reference: "VIDEO-147",
			trackers: []Tracker{{
				ProjectID:  "detent",
				Kind:       "github",
				Repository: "digitaldrywood/detent",
			}},
			wantErr: "cannot resolve non-GitHub pause exit issue",
		},
		{
			name:      "cross project GitHub reference uses owning tracker",
			pausedID:  "video",
			reference: "digitaldrywood/video-studio#147",
			trackers: []Tracker{
				{ProjectID: "video", Kind: "local_sqlite"},
				{ProjectID: "video-studio", Kind: "github", Repository: "digitaldrywood/video-studio"},
			},
			want: ReferenceResolution{
				ProjectID:  "video-studio",
				Reference:  "digitaldrywood/video-studio#147",
				Repository: "digitaldrywood/video-studio",
			},
		},
		{
			name:      "repository shorthand uses owning tracker",
			pausedID:  "video",
			reference: "video-studio#147",
			trackers: []Tracker{
				{ProjectID: "video", Kind: "local_sqlite"},
				{ProjectID: "video-studio", Kind: "github", Repository: "digitaldrywood/video-studio"},
			},
			want: ReferenceResolution{
				ProjectID:  "video-studio",
				Reference:  "digitaldrywood/video-studio#147",
				Repository: "digitaldrywood/video-studio",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveReference(tt.pausedID, tt.reference, tt.trackers)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveReference() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveReference() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ResolveReference() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
