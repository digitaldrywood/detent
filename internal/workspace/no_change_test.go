package workspace

import (
	"strings"
	"testing"
)

func TestCurrentBaseIgnoresStaleTrackingRefs(t *testing.T) {
	for _, test := range []struct {
		name    string
		branch  string
		advance bool
		missing bool
	}{
		{name: "configured branch", branch: "main"},
		{name: "remote default branch"},
		{name: "configured branch advances", branch: "main", advance: true},
		{name: "remote default advances", advance: true},
		{name: "missing configured branch", branch: "missing", missing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLandingFixture(t)
			f.issue.ProgressBaseRef = test.branch
			before := runGit(t, f.info.Path, "rev-parse", "HEAD")
			tracking := runGit(t, f.info.Path, "rev-parse", "refs/remotes/origin/main")
			want := strings.TrimSpace(tracking)
			if test.advance {
				runGit(t, f.source, "commit", "--allow-empty", "-m", "advance base")
				want = strings.TrimSpace(runGit(t, f.source, "rev-parse", "HEAD"))
				runGit(t, f.source, "push", "origin", "HEAD:main")
				runGit(t, f.info.Path, "update-ref", "refs/remotes/origin/main", strings.TrimSpace(tracking))
			}
			got, err := f.backend.CurrentBase(t.Context(), f.info, f.issue)
			if test.missing {
				if err == nil || got != "" {
					t.Fatalf("missing base = %q, %v", got, err)
				}
			} else if err != nil || got != want {
				t.Fatalf("base = %q, %v; want %q", got, err, want)
			}
			if runGit(t, f.info.Path, "rev-parse", "HEAD") != before || runGit(t, f.info.Path, "rev-parse", "refs/remotes/origin/main") != tracking {
				t.Fatal("observing the remote base mutated local refs")
			}
		})
	}
}
