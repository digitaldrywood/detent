package platformstore

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func TestGitHubIngressContinuesPastFailureAndResumesAfterRestart(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	ctx := t.Context()
	seedOrganizations(t, r)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	const repo = "owner/repository"
	run := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"org_first", "org_second"} {
		run(`INSERT INTO public.organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, id, now)
	}
	run(`UPDATE platform.organizations SET generation=3 WHERE id='org_second'`)
	for _, project := range []struct{ org, id, profile, repository, checkout string }{
		{"org_first", "a", "native", repo, repo},
		{"org_first", "b", "github_compatible", repo, ""},
		{"org_second", "c", "native", "", "OWNER/REPOSITORY"},
		{"org_second", "d", "native", "other/repository", ""},
	} {
		run(`INSERT INTO public.projects(organization_id,id,name,profile) VALUES($1,$2,$2,$3)`, project.org, project.id, project.profile)
		run(`INSERT INTO public.project_settings(organization_id,project_id,repository,checkout_repository) VALUES($1,$2,$3,$4)`, project.org, project.id, project.repository, project.checkout)
	}
	g := GitHubIngress{DeliveryID: "delivery-a", EventType: "issues", Repository: repo, Digest: strings.Repeat("a", 64), ObjectKey: "object-a"}
	if ok, err := r.BeginGitHubIngress(ctx, &g, now); err != nil || ok {
		t.Fatalf("observed absent ingress=%t error=%v", ok, err)
	}
	if ok, err := r.AcceptGitHubIngress(ctx, g, now); err != nil || !ok {
		t.Fatalf("accepted=%t error=%v", ok, err)
	}
	ping := g
	ping.DeliveryID, ping.EventType, ping.Repository, ping.ObjectKey = "delivery-b", "ping", "", "object-b"
	if ok, err := r.BeginGitHubIngress(ctx, &ping, now); err != nil || ok {
		t.Fatalf("begin ping=%t error=%v", ok, err)
	}
	if ok, err := r.AcceptGitHubIngress(ctx, ping, now); err != nil || !ok {
		t.Fatalf("accepted ping=%t error=%v", ok, err)
	}
	for _, change := range []struct {
		name     string
		change   func(*GitHubIngress)
		released bool
	}{
		{"event", func(g *GitHubIngress) { g.EventType = "pull_request" }, false},
		{"repository", func(g *GitHubIngress) { g.Repository = "other/repository" }, false},
		{"digest", func(g *GitHubIngress) { g.Digest = strings.Repeat("b", 64) }, true},
	} {
		t.Run(change.name, func(t *testing.T) {
			diff := g
			change.change(&diff)
			for _, write := range []func() (bool, error){
				func() (bool, error) { return r.BeginGitHubIngress(ctx, &diff, now.Add(time.Hour)) },
				func() (bool, error) { return r.AcceptGitHubIngress(ctx, diff, now.Add(time.Hour)) },
			} {
				if ok, err := write(); ok || !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("conflict=%t error=%v", ok, err)
				}
			}
			if released, err := r.GitHubIngressReleased(ctx, diff.DeliveryID, diff.Digest, diff.ObjectKey); err != nil || released != change.released {
				t.Fatalf("released=%t error=%v", released, err)
			}
		})
	}
	first := hubmaintenance.Tenant{Organization: "org_first", Generation: 1}
	second := hubmaintenance.Tenant{Organization: "org_second", Generation: 3}
	next := func(want GitHubIngress, cleanup bool, targets ...hubmaintenance.Tenant) *GitHubFanoutPage {
		t.Helper()
		page, err := r.NextGitHubFanout(ctx, now, 1)
		if err != nil || page == nil || page.GitHubIngress != want || page.Cleanup != cleanup || !slices.Equal(page.Targets, targets) {
			t.Fatalf("at %v want %s cleanup=%t targets=%v, got %+v error=%v", now, want.DeliveryID, cleanup, targets, page, err)
		}
		return page
	}
	advance := func(p *GitHubFanoutPage, failed bool) {
		t.Helper()
		if err := r.AdvanceGitHubFanout(ctx, p, failed, now); err != nil {
			t.Fatal(err)
		}
	}
	failed := next(g, false, first)
	stale := next(g, false, first)
	advance(failed, true)
	if err := r.AdvanceGitHubFanout(ctx, stale, false, now); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("stale advance: %v", err)
	}
	advance(next(ping, false), false)
	advance(next(g, false), false)
	advance(next(ping, true), true)
	r = repository(t, db, testConfig(t))
	advance(next(g, false, second), false)
	if page, err := r.NextGitHubFanout(ctx, now, 1); err != nil || page != nil {
		t.Fatalf("failed work immediately due: %+v %v", page, err)
	}
	run(`INSERT INTO public.github_webhook_receipts(organization_id,binding_project,delivery_id,repository,event_type,action,payload_digest,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,received_at,expires_at,next_attempt_at)
	VALUES('org_second','c',$1,$2,$3,'',$4,'','',0,0,$5,$5,$5)`, g.DeliveryID, g.Repository, g.EventType, g.Digest, now)
	now = now.Add(time.Minute)
	for range 2 {
		page, err := r.NextGitHubFanout(ctx, now, 1)
		if err != nil || page == nil {
			t.Fatalf("retry page=%+v error=%v", page, err)
		}
		switch page.DeliveryID {
		case ping.DeliveryID:
			if !page.Cleanup {
				t.Fatalf("lost pending deletion: %+v", page)
			}
		case g.DeliveryID:
			if !slices.Equal(page.Targets, []hubmaintenance.Tenant{first}) {
				t.Fatalf("failed recipient lost: %+v", page)
			}
		default:
			t.Fatalf("unexpected retry: %+v", page)
		}
		advance(page, false)
	}
	advance(next(g, false), false)
	advance(next(g, false), false)
	if released, err := r.GitHubIngressReleased(ctx, g.DeliveryID, g.Digest, g.ObjectKey); err != nil || released {
		t.Fatalf("released before cleanup: %t %v", released, err)
	}
	advance(next(g, true), false)
	if released, err := r.GitHubIngressReleased(ctx, g.DeliveryID, g.Digest, g.ObjectKey); err != nil || !released {
		t.Fatalf("released after cleanup: %t %v", released, err)
	}
	if ok, err := r.AcceptGitHubIngress(ctx, g, now); err != nil || ok {
		t.Fatalf("duplicate accepted=%t error=%v", ok, err)
	}
	now = now.Add(time.Minute)
	if ok, err := r.BeginGitHubIngress(ctx, &g, now); err != nil || !ok {
		t.Fatalf("redelivery observed=%t error=%v", ok, err)
	}
	if ts, err := r.LatestGitHubIngress(ctx); err != nil || !ts.Equal(now) {
		t.Fatalf("latest ingress=%v error=%v", ts, err)
	}
	if page, err := r.NextGitHubFanout(ctx, now, 2); err != nil || page != nil {
		t.Fatalf("completed redelivery restarted: %+v %v", page, err)
	}
}

func TestGitHubIngressRequiresBindingAtAcceptanceAndHandoff(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	ctx := t.Context()
	seedOrganizations(t, r)
	run := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	setup := func(org, repository, checkout string) {
		t.Helper()
		run(`INSERT INTO public.organizations(organization_id,name,local,created_at) VALUES($1,$1,false,now())`, org)
		run(`INSERT INTO public.hosted_tenant(organization_id,provider_id,bootstrap_subject,deployment,public_url,allocation_generation)
			VALUES($1,$1,'operator','hosted','https://example.test',1)`, org)
		run(`INSERT INTO public.projects(organization_id,id,name,profile) VALUES($1,'p','example','native')`, org)
		run(`INSERT INTO public.project_settings(organization_id,project_id,repository,checkout_repository) VALUES($1,'p',$2,$3)`, org, repository, checkout)
	}
	const repositoryName = "owner/repository"
	setup("org_first", repositoryName, "")
	setup("org_second", "", "")
	beforeAcceptance, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer beforeAcceptance.Rollback()
	if _, err := beforeAcceptance.ExecContext(ctx, `UPDATE public.project_settings SET repository=$1 WHERE organization_id='org_second'`, repositoryName); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	event := GitHubIngress{DeliveryID: "private", EventType: "issues", Digest: strings.Repeat("a", 64), Repository: repositoryName, ObjectKey: "object"}
	if ok, err := r.BeginGitHubIngress(ctx, &event, now); err != nil || ok {
		t.Fatalf("begin=%t error=%v", ok, err)
	}
	if ok, err := r.AcceptGitHubIngress(ctx, event, now); err != nil || !ok {
		t.Fatalf("accept=%t error=%v", ok, err)
	}
	if err := beforeAcceptance.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register(ctx, Organization{ID: "org_third", ProviderID: "third", Name: "Third", Generation: 1, State: "ready", Endpoint: "unix:/example/third.sock"}); err != nil {
		t.Fatal(err)
	}
	setup("org_third", repositoryName, "")
	run(`UPDATE public.project_settings SET require_visual_attachments=true, checkout_repository=$1 WHERE organization_id='org_first'`, repositoryName)
	run(`UPDATE public.projects SET name='renamed' WHERE organization_id='org_first' AND id='p'`)
	run(`UPDATE platform.organizations SET name='renamed' WHERE id='org_first'`)
	p, err := r.NextGitHubFanout(ctx, now, 8)
	if err != nil || p == nil || !slices.Equal(p.Targets, []hubmaintenance.Tenant{{Organization: "org_first", Generation: 1}}) || p.BindingProject != "p" {
		t.Fatalf("original binding after unrelated edits: %+v %v", p, err)
	}
	pool := rolePool(t, db, postgres.RuntimeRoleName, "")
	authorized := func(org string, want bool) {
		t.Helper()
		for _, lock := range []bool{false, true} {
			mode := pgx.ReadOnly
			if lock {
				mode = pgx.ReadWrite
			}
			tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: mode})
			if err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					if err := tx.Rollback(ctx); err != nil {
						t.Error(err)
					}
				}()
				if _, err := tx.Exec(ctx, `SELECT set_config('detent.organization_id',$1,true)`, org); err != nil {
					t.Fatal(err)
				}
				var got bool
				if err := tx.QueryRow(ctx, `SELECT public.hosted_github_webhook_authorized($1,'p',$2,$3,$4,$5)`, event.DeliveryID, event.Repository, event.EventType, event.Digest, lock).Scan(&got); err != nil || got != want {
					t.Fatalf("organization=%s lock=%t authorized=%t want=%t error=%v", org, lock, got, want, err)
				}
			}()
		}
	}
	authorized("org_first", true)
	authorized("org_second", false)
	authorized("org_third", false)
	run(`UPDATE public.project_settings SET repository='' WHERE organization_id='org_first'`)
	authorized("org_first", false)
	if err := r.AdvanceGitHubFanout(ctx, p, false, now); err != nil {
		t.Fatal(err)
	}
	p, err = r.NextGitHubFanout(ctx, now, 8)
	if err != nil || p == nil || len(p.Targets) != 0 || p.BindingProject != "" {
		t.Fatalf("late bindings: %+v %v", p, err)
	}
}

func TestGitHubIngressRecoversStagedPublication(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	ctx := t.Context()
	g := GitHubIngress{DeliveryID: "delivery", EventType: "issues", Repository: "owner/repository", Digest: strings.Repeat("a", 64), ObjectKey: "object"}
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if _, err := r.AcceptGitHubIngress(ctx, g, at); !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("accepted without reservation: %v", err)
	}
	begin := func(want bool, when time.Time) {
		t.Helper()
		if accepted, err := r.BeginGitHubIngress(ctx, &g, when); err != nil || accepted != want {
			t.Fatalf("already accepted=%t want=%t error=%v", accepted, want, err)
		}
	}
	begin(false, at)
	if _, err := db.ExecContext(ctx, `ALTER TABLE platform.github_webhook_ingress ADD CONSTRAINT reject_promotion CHECK (state<>'routing') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AcceptGitHubIngress(ctx, g, at); err == nil {
		t.Fatal("rejected promotion succeeded")
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE platform.github_webhook_ingress DROP CONSTRAINT reject_promotion`); err != nil {
		t.Fatal(err)
	}
	begin(false, at.Add(time.Second))
	r = repository(t, db, testConfig(t))
	now := at.Add(time.Minute)
	staged := func() *GitHubFanoutPage {
		t.Helper()
		p, err := r.NextGitHubFanout(ctx, now, 1)
		if err != nil || p == nil || !p.Staging || p.Cleanup || len(p.Targets) != 0 || p.ObjectKey != g.ObjectKey {
			t.Fatalf("recovering reservation: %+v %v", p, err)
		}
		return p
	}
	if latest, err := r.LatestGitHubIngress(ctx); err != nil || !latest.IsZero() {
		t.Fatalf("unpublished replay anchor=%v error=%v", latest, err)
	}
	abandoned := staged()
	firstKey := g.ObjectKey
	if err := r.ReleaseGitHubStaging(ctx, abandoned); err != nil {
		t.Fatal(err)
	}
	if page, err := r.NextGitHubFanout(ctx, now.Add(24*time.Hour), 1); page != nil || err != nil {
		t.Fatalf("abandoned upload remained due: %+v %v", page, err)
	}
	g.ObjectKey = "replacement-object"
	begin(false, at.Add(time.Second))
	if promoted, err := r.AcceptGitHubIngress(ctx, abandoned.GitHubIngress, now); promoted || !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("superseded upload promoted=%t error=%v", promoted, err)
	}
	if err := r.ReleaseGitHubStaging(ctx, abandoned); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("old reservation released replacement: %v", err)
	}
	if released, err := r.GitHubIngressReleased(ctx, g.DeliveryID, g.Digest, firstKey); err != nil || !released {
		t.Fatalf("superseded upload released=%t error=%v", released, err)
	}
	if released, err := r.GitHubIngressReleased(ctx, g.DeliveryID, g.Digest, g.ObjectKey); err != nil || released {
		t.Fatalf("current upload released=%t error=%v", released, err)
	}
	if err := r.AdvanceGitHubFanout(ctx, staged(), true, now); err != nil {
		t.Fatal(err)
	}
	if p, err := r.NextGitHubFanout(ctx, now, 1); p != nil || err != nil {
		t.Fatalf("missing payload retried immediately: %+v %v", p, err)
	}
	now = now.Add(time.Minute)
	p := staged()
	if err := r.AdvanceGitHubFanout(ctx, p, false, now); err != nil {
		t.Fatal(err)
	}
	if err := r.AdvanceGitHubFanout(ctx, p, true, now); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("stale staging attempted to defer verified payload: %v", err)
	}
	if latest, err := r.LatestGitHubIngress(ctx); err != nil || !latest.Equal(at.Add(time.Second)) {
		t.Fatalf("background promotion moved replay anchor: %v %v", latest, err)
	}
	if promoted, err := r.AcceptGitHubIngress(ctx, g, now); err != nil || promoted {
		t.Fatalf("repeated promotion=%t error=%v", promoted, err)
	}
	begin(true, now)
	page, err := r.NextGitHubFanout(ctx, now, 1)
	if err != nil || page == nil || page.Staging || page.Cleanup {
		t.Fatalf("reconciled delivery unavailable for routing: %+v %v", page, err)
	}
}
