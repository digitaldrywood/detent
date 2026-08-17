package forgeavailability

import "testing"

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		operation string
		detail    string
		wantClass string
		want      bool
	}{
		{name: "git HTTP 503", operation: "git push", detail: "fatal: unable to access repository: HTTP 503", wantClass: ClassServer, want: true},
		{name: "git timeout", operation: "git push", detail: "ssh: connect to host github.com port 22: Operation timed out", wantClass: ClassTimeout, want: true},
		{name: "git DNS", operation: "git push", detail: "ssh: Could not resolve hostname github.com: no such host\nfatal: Could not read from remote repository.", wantClass: ClassTransport, want: true},
		{name: "git fetch server error", operation: "git fetch", detail: "remote: HTTP 503 Service Unavailable", wantClass: ClassServer, want: true},
		{name: "pull request server error", operation: "codex_apps/github.create_pull_request", detail: `{"status":502,"message":"unavailable"}`, wantClass: ClassServer, want: true},
		{name: "non fast forward", operation: "git push", detail: "[rejected] main -> main (non-fast-forward)"},
		{name: "protected branch", operation: "git push", detail: "remote: protected branch hook declined"},
		{name: "forbidden", operation: "git push", detail: "HTTP 403: forbidden"},
		{name: "credential capacity", operation: "gh pr create", detail: "HTTP 429: too many requests"},
		{name: "lookup is not a write", operation: "codex_apps/github.search_issues", detail: "HTTP 503"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotClass, got := Classify(tt.operation, tt.detail)
			if got != tt.want || gotClass != tt.wantClass {
				t.Fatalf("Classify() = %q, %v, want %q, %v", gotClass, got, tt.wantClass, tt.want)
			}
		})
	}
}

func TestProvesReachability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		operation string
		detail    string
		want      bool
	}{
		{name: "non fast forward", operation: "git push", detail: "[rejected] feature -> feature (non-fast-forward)", want: true},
		{name: "forbidden", operation: "git push", detail: "HTTP 403: forbidden", want: true},
		{name: "rate limited", operation: "gh pr create", detail: "HTTP 429: too many requests", want: true},
		{name: "server unavailable", operation: "git push", detail: "HTTP 503: unavailable"},
		{name: "transport failure", operation: "git push", detail: "Could not resolve host: github.com"},
		{name: "ambiguous failure", operation: "git push", detail: "command exited with status 1"},
		{name: "read response", operation: "search issues", detail: "HTTP 403: forbidden"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ProvesReachability(tt.operation, tt.detail); got != tt.want {
				t.Fatalf("ProvesReachability() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHostNormalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		call  func(string) string
		want  string
	}{
		{name: "GitHub API endpoint", value: "https://api.github.com/graphql", call: HostFromEndpoint, want: "github.com"},
		{name: "enterprise endpoint", value: "https://forge.example.com/api/graphql", call: HostFromEndpoint, want: "forge.example.com"},
		{name: "HTTPS remote", value: "fatal: unable to access https://github.com/acme/widgets.git", call: HostFromText, want: "github.com"},
		{name: "SSH remote", value: "git@forge.example.com:acme/widgets.git", call: HostFromText, want: "forge.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.call(tt.value); got != tt.want {
				t.Fatalf("host = %q, want %q", got, tt.want)
			}
		})
	}
}
