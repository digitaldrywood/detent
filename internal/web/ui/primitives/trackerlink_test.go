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
			if err := TrackerReferenceLink("DD-1132", tt.url, "font-mono").Render(context.Background(), &output); err != nil {
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
