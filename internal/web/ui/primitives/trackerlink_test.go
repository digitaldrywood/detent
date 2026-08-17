package primitives

import (
	"context"
	"strings"
	"testing"
)

func TestTrackerReferenceLink(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		url       string
		isolated  bool
		want      []string
		forbidden []string
	}{
		{
			name: "linked reference",
			url:  "https://github.com/digitaldrywood/detent/issues/1132",
			want: []string{
				`href="https://github.com/digitaldrywood/detent/issues/1132"`,
				`target="_blank"`,
				`rel="noopener noreferrer"`,
			},
			forbidden: []string{`onclick="event.stopPropagation()"`},
		},
		{
			name:     "isolated linked reference",
			url:      "https://github.com/digitaldrywood/detent/pull/1132",
			isolated: true,
			want:     []string{`onclick="event.stopPropagation()"`},
		},
		{
			name:      "plain fallback",
			want:      []string{`class="font-mono">DD-1132</span>`},
			forbidden: []string{"<a "},
		},
		{
			name:      "unsafe URL",
			url:       "javascript:alert(1)",
			forbidden: []string{`href="javascript:alert(1)"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var output strings.Builder
			component := TrackerReferenceLink("DD-1132", tt.url, "font-mono")
			if tt.isolated {
				component = TrackerReferenceLinkIsolated("DD-1132", tt.url, "font-mono")
			}
			if err := component.Render(context.Background(), &output); err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			html := output.String()
			for _, want := range tt.want {
				if !strings.Contains(html, want) {
					t.Fatalf("rendered link missing %q:\n%s", want, html)
				}
			}
			for _, forbidden := range tt.forbidden {
				if strings.Contains(html, forbidden) {
					t.Fatalf("rendered link contains %q:\n%s", forbidden, html)
				}
			}
		})
	}
}
