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
