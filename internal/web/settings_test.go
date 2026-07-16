package web

import "testing"

func TestWorkflowDetailsURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		projectURL string
		want       string
	}{
		{
			name:       "organization project",
			projectURL: "https://github.com/orgs/digitaldrywood/projects/4",
			want:       "https://github.com/orgs/digitaldrywood/projects/4/workflows",
		},
		{
			name:       "user project view",
			projectURL: "https://github.com/users/octocat/projects/12/views/3?pane=info#details",
			want:       "https://github.com/users/octocat/projects/12/workflows",
		},
		{name: "project node id", projectURL: "PVT_kwDOExample"},
		{name: "repository slug", projectURL: "digitaldrywood/detent"},
		{name: "non-project URL", projectURL: "https://github.com/digitaldrywood/detent"},
		{name: "invalid project number", projectURL: "https://github.com/orgs/digitaldrywood/projects/not-a-number"},
		{name: "missing URL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := workflowDetailsURL(tt.projectURL); got != tt.want {
				t.Fatalf("workflowDetailsURL(%q) = %q, want %q", tt.projectURL, got, tt.want)
			}
		})
	}
}
