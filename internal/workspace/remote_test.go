package workspace

import "testing"

func TestHTTPSRemoteURL(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		remote string
		want   string
	}{
		{name: "https", remote: "https://github.com/example/repo.git", want: "https://github.com/example/repo"},
		{name: "https without suffix", remote: "https://gitlab.example.com/group/sub/repo", want: "https://gitlab.example.com/group/sub/repo"},
		{name: "https with trailing slash", remote: "https://github.com/example/repo/", want: "https://github.com/example/repo"},
		{name: "scp like ssh", remote: "git@github.com:example/repo.git", want: "https://github.com/example/repo"},
		{name: "ssh scheme", remote: "ssh://git@github.com/example/repo.git", want: "https://github.com/example/repo"},
		{name: "ssh scheme with port", remote: "ssh://git@git.example.com:2222/team/repo.git", want: "https://git.example.com/team/repo"},
		{name: "https with credentials", remote: "https://token@github.com/example/repo.git", want: ""},
		{name: "https with query", remote: "https://github.com/example/repo?x=1", want: ""},
		{name: "http", remote: "http://github.com/example/repo.git", want: ""},
		{name: "local path", remote: "/srv/git/repo.git", want: ""},
		{name: "relative path", remote: "../origin.git", want: ""},
		{name: "file scheme", remote: "file:///srv/git/repo.git", want: ""},
		{name: "windows path", remote: `C:\git\repo`, want: ""},
		{name: "blank", remote: "  ", want: ""},
		{name: "host only", remote: "git@github.com:", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := HTTPSRemoteURL(test.remote); got != test.want {
				t.Fatalf("HTTPSRemoteURL(%q) = %q, want %q", test.remote, got, test.want)
			}
		})
	}
}
