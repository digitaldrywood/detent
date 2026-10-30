package workspace

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPublicationReconciliationBeforeForgeRead(t *testing.T) {
	for _, test := range []struct {
		name        string
		remote      string
		external    bool
		wantEffects []string
	}{
		{name: "intended SHA records publication before unavailable PR read", remote: "intended", wantEffects: []string{"git_push:confirmed"}},
		{name: "absent branch permits reconciliation despite recorded old head"},
		{name: "absent retained PR branch permits uncertain publication reconciliation", external: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLandingFixture(t)
			repository := "https://github.com/example/repo"
			runGit(t, f.source, "config", "url.file://"+f.remote+".insteadOf", repository+".git")
			runGit(t, f.source, "remote", "set-url", "origin", repository+".git")
			if test.remote == "intended" {
				runGit(t, f.info.Path, "push", "origin", f.head+":refs/heads/"+f.info.Branch)
			}
			before := runGit(t, f.remote, "for-each-ref", "--format=%(refname) %(objectname)")
			lookupErr := errors.New("PR lookup unavailable")
			reads := 0
			client, err := github.NewClient(github.ClientConfig{TokenSource: github.StaticTokenSource("test-publication"), DisableConditionalRequests: true, HTTPClient: landingHTTPClient(func(*http.Request) (*http.Response, error) {
				reads++
				return nil, lookupErr
			})})
			if err != nil {
				t.Fatal(err)
			}
			var effects []string
			opts := LandOptions{Repository: repository, HeadSHA: f.head, GitHubClient: client, UnresolvedEffect: true, ExpectedPublicationHead: f.remoteMain(t), PublicationEffect: func(_ context.Context, kind, state string, _ GitHubPublication) error {
				effects = append(effects, kind+":"+state)
				return nil
			}}
			if test.external {
				opts.External = &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: repository + "/pull/7"}
				opts.ExternalHeadSHA = opts.ExpectedPublicationHead
			}
			_, _, _, err = f.backend.publishGitHubPull(t.Context(), f.info, f.issue, opts, "example/repo", "example", "origin", f.info.Branch, f.head, "main")
			if !errors.Is(err, lookupErr) || reads != 1 || !reflect.DeepEqual(effects, test.wantEffects) {
				t.Fatalf("reconciliation lost remote evidence: effects=%v reads=%d error=%v", effects, reads, err)
			}
			if after := runGit(t, f.remote, "for-each-ref", "--format=%(refname) %(objectname)"); after != before {
				t.Fatal("reconciliation changed remote refs before forge evidence was available")
			}
		})
	}
}
