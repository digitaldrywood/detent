//go:build !windows

package cloudentry

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSharedOriginPilotPreview(t *testing.T) {
	directory := os.Getenv("DETENT_SHARED_ORIGIN_PREVIEW")
	if directory == "" {
		t.Fatal("set DETENT_SHARED_ORIGIN_PREVIEW to a private directory to serve the isolated shared-origin browser preview")
	}
	p := newSharedOriginPilot(t, 3, 3)
	stop := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__preview/authorize", p.previewAuthorize)
	mux.HandleFunc("POST /__preview/stop", func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(stop) })
		w.WriteHeader(http.StatusNoContent)
	})
	service := p.service.Handler()
	mux.Handle("/", service)
	var handler http.Handler = mux
	p.handler.handler.Store(&handler)
	fixture := seedSharedOriginPreview(t, p)
	p.provider.mu.Lock()
	p.provider.authorizeBase = p.base
	p.provider.mu.Unlock()
	raw, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "shared-origin-preview.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("shared-origin preview: %s", p.base)
	timer := time.NewTimer(20 * time.Minute)
	defer timer.Stop()
	select {
	case <-stop:
	case <-timer.C:
	case <-t.Context().Done():
	}
}

func (p *sharedOriginPilot) previewAuthorize(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	p.provider.mu.Lock()
	type choice struct{ label, code string }
	var choices []choice
	for _, user := range []string{"member", "eve", "fay", "gus", "hal"} {
		subject := "user_" + user
		if _, ok := p.provider.users[subject]; !ok {
			continue
		}
		choices = append(choices, choice{user + "@example.test (account)", subject + ":"})
		for _, membership := range p.provider.memberships {
			if membership.UserID == subject {
				choices = append(choices, choice{user + "@example.test into " + membership.OrganizationID, subject + ":" + membership.OrganizationID})
			}
		}
	}
	p.provider.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var page strings.Builder
	page.WriteString(`<!doctype html><html><head><meta name="viewport" content="width=device-width"><title>Fixture identity provider</title></head><body style="font-family:sans-serif;padding:16px"><h1>Fixture identity provider</h1><p>Synthetic accounts only. No WorkOS request is made.</p><ul>`)
	for _, item := range choices {
		fmt.Fprintf(&page, `<li><a href="/auth/oidc/callback?%s">%s</a></li>`, url.Values{"code": {item.code}, "state": {state}}.Encode(), item.label)
	}
	page.WriteString(`</ul></body></html>`)
	_, _ = io.WriteString(w, page.String())
}

func seedSharedOriginPreview(t *testing.T, p *sharedOriginPilot) map[string]string {
	t.Helper()
	organizations := map[string]pilotOrganization{}
	for _, seed := range []struct{ user, name, project string }{{"member", "Alpha Labs", "Alpha secret project"}, {"eve", "Beta Works", "Beta secret project"}} {
		owner := p.browser(t)
		owner.login("/auth/oidc/start", "user_"+seed.user+":")
		created := owner.createOrganization(seed.name)
		id := organizationFromLocation(t, created.location)
		p.waitState(t, id, "ready")
		owner.login("/organizations/"+id+"/organization", "user_"+seed.user+":porg_"+id)
		o := pilotOrganization{id: id, owner: owner, projectName: seed.project}
		o.ownerCSRF = owner.csrf(o.page())
		project := owner.form("/organizations/"+id+"/projects", url.Values{"name": {seed.project}, "grant_access": {"true"}, "csrf": {o.ownerCSRF}})
		o.project = strings.TrimPrefix(project.location, "/organizations/"+id+"/projects/")
		pilotStatus(t, "invite", owner.form("/organizations/"+id+"/organization/invite", url.Values{"email": {"fay@example.test"}, "role": {"member"}, "csrf": {o.ownerCSRF}}), http.StatusSeeOther)
		fay := p.browser(t)
		start := fay.get("/invite?invitation_token=inv_fay")
		provider, err := url.Parse(start.location)
		if err != nil {
			t.Fatal(err)
		}
		fay.get("/auth/oidc/callback?" + url.Values{"code": {"user_fay:"}, "state": {provider.Query().Get("state")}}.Encode())
		pilotStatus(t, "grant", owner.form("/organizations/"+id+"/organization/grants", url.Values{"user": {"user_fay"}, "project": {o.project}, "write": {"true"}, "csrf": {o.ownerCSRF}}), http.StatusSeeOther)
		issue := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "seed"}, Title: seed.name + " native issue", Body: issueContractTestSections, State: "Todo"}
		pilotStatus(t, "issue", owner.json(http.MethodPost, o.projectAPI()+"/work-items", o.ownerCSRF, issue), http.StatusOK)
		organizations[seed.user] = o
	}
	p.provider.mu.Lock()
	p.provider.users["user_hal"] = "hal@example.test"
	p.provider.mu.Unlock()
	alpha, beta := organizations["member"], organizations["eve"]
	return map[string]string{
		"origin": p.base, "alpha": alpha.id, "beta": beta.id,
		"alpha_project": p.base + "/organizations/" + alpha.id + "/projects/" + alpha.project,
		"beta_project":  p.base + "/organizations/" + beta.id + "/projects/" + beta.project,
		"stop":          p.base + "/__preview/stop",
	}
}
