package postgres_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDuplicateChallengesAreScopedAndConsumedAtomically(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	admin.SetMaxOpenConns(2)
	now := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example organization", Slug: string(org), Local: true, CreatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		seedRepository(t, repository, org)
		for i, tokenPart := range []string{"a", "b"} {
			created := now.Add(-2 * time.Hour)
			if i == 1 {
				created = now
			}
			challenge := corestore.DuplicateChallenge{OrganizationID: org, ProjectID: "prj_first", Token: "dup_" + strings.Repeat(tokenPart, 32), PrincipalID: "example-principal", RequestID: "example-request", ContentHash: strings.Repeat("a", 64), PolicyHash: strings.Repeat("d", 64), SourceSequence: 2, CreatedAt: created, ExpiresAt: created.Add(time.Hour), Candidates: []corestore.DuplicateCandidate{{WorkItemID: "wi_first_1", Number: 1, Revision: 1}, {WorkItemID: "wi_first_2", Number: 2, Revision: 1}}}
			if err := repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
				return w.(corestore.DuplicateChallengeWriter).CreateDuplicateChallenge(t.Context(), challenge)
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.Read(t.Context(), org, corestore.Serving, func(r corestore.Reader) error {
				got, err := r.(corestore.DuplicateChallengeReader).DuplicateChallenge(t.Context(), "prj_first", challenge.Token)
				if err == nil && !reflect.DeepEqual(got, challenge) {
					t.Errorf("challenge = %+v, want %+v", got, challenge)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	identity := corestore.DuplicateChallenge{OrganizationID: "org_first", ProjectID: "prj_first", Token: "dup_" + strings.Repeat("b", 32), PrincipalID: "example-principal", RequestID: "example-request", ContentHash: strings.Repeat("a", 64), PolicyHash: strings.Repeat("d", 64), SourceSequence: 2}
	for _, test := range []struct {
		name       string
		candidates []corestore.DuplicateCandidate
	}{
		{"duplicate IDs", []corestore.DuplicateCandidate{{WorkItemID: "wi_first_1", Number: 1, Revision: 1}, {WorkItemID: "wi_first_1", Number: 2, Revision: 1}}},
		{"duplicate numbers", []corestore.DuplicateCandidate{{WorkItemID: "wi_first_1", Number: 1, Revision: 1}, {WorkItemID: "wi_first_2", Number: 1, Revision: 1}}},
		{"invalid revision", []corestore.DuplicateCandidate{{WorkItemID: "wi_first_1", Number: 1}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := identity
			invalid.Token = "dup_" + strings.Repeat("c", 32)
			invalid.PolicyHash = strings.Repeat("d", 64)
			invalid.CreatedAt, invalid.ExpiresAt, invalid.Candidates = now, now.Add(time.Hour), test.candidates
			if err := repository.Write(t.Context(), "org_first", corestore.Serving, func(w corestore.Writer) error {
				return w.(corestore.DuplicateChallengeWriter).CreateDuplicateChallenge(t.Context(), invalid)
			}); err == nil {
				t.Fatal("invalid candidate set was accepted")
			}
		})
	}
	for _, test := range []struct {
		name   string
		mutate func(*corestore.DuplicateChallenge)
	}{
		{"principal", func(c *corestore.DuplicateChallenge) { c.PrincipalID = "other-principal" }},
		{"request", func(c *corestore.DuplicateChallenge) { c.RequestID = "other-request" }},
		{"content", func(c *corestore.DuplicateChallenge) { c.ContentHash = strings.Repeat("b", 64) }},
		{"policy", func(c *corestore.DuplicateChallenge) { c.PolicyHash = strings.Repeat("c", 64) }},
		{"source sequence", func(c *corestore.DuplicateChallenge) { c.SourceSequence++ }},
		{"expired", func(c *corestore.DuplicateChallenge) { c.Token = "dup_" + strings.Repeat("a", 32) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			wrong := identity
			test.mutate(&wrong)
			err := repository.Write(t.Context(), "org_first", corestore.Serving, func(w corestore.Writer) error {
				_, err := w.(corestore.DuplicateChallengeWriter).ConsumeDuplicateChallenge(t.Context(), wrong, now)
				return err
			})
			if !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("mismatched consumption = %v, want not found", err)
			}
		})
	}
	rollback := errors.New("rollback example")
	err := repository.Write(t.Context(), "org_first", corestore.Serving, func(w corestore.Writer) error {
		if _, err := w.(corestore.DuplicateChallengeWriter).ConsumeDuplicateChallenge(t.Context(), identity, now); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("rollback = %v", err)
	}
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			ready <- struct{}{}
			<-start
			results <- repository.Write(t.Context(), "org_first", corestore.Serving, func(w corestore.Writer) error {
				_, err := w.(corestore.DuplicateChallengeWriter).ConsumeDuplicateChallenge(t.Context(), identity, now)
				return err
			})
		}()
	}
	<-ready
	<-ready
	close(start)
	success, missing := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			success++
		case errors.Is(err, corestore.ErrNotFound):
			missing++
		default:
			t.Errorf("concurrent consumption = %v", err)
		}
	}
	if success != 1 || missing != 1 {
		t.Fatalf("concurrent consumption: %d succeeded, %d missing", success, missing)
	}
	later := identity
	later.Token = "dup_" + strings.Repeat("c", 32)
	later.CreatedAt = now.Add(-90 * time.Minute)
	later.ExpiresAt = later.CreatedAt.Add(time.Hour)
	later.Candidates = []corestore.DuplicateCandidate{{WorkItemID: "wi_first_1", Number: 1, Revision: 1}}
	if err := repository.Write(t.Context(), "org_first", corestore.Serving, func(w corestore.Writer) error {
		return w.(corestore.DuplicateChallengeWriter).CreateDuplicateChallenge(t.Context(), later)
	}); err != nil {
		t.Fatal(err)
	}
	lock, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Rollback() })
	if _, err := lock.ExecContext(t.Context(), `SELECT token FROM native_duplicate_challenges WHERE organization_id=$1 AND project_id=$2 AND token=$3 FOR UPDATE`, "org_first", "prj_first", "dup_"+strings.Repeat("a", 32)); err != nil {
		t.Fatal(err)
	}
	pruned := make(chan error, 1)
	go func() {
		pruned <- repository.Write(t.Context(), "org_first", corestore.Background, func(w corestore.Writer) error {
			count, err := w.(corestore.DuplicateChallengeWriter).PruneDuplicateChallenges(t.Context(), now, 1)
			if err == nil && count != 0 {
				return errors.New("pruning advanced past a locked candidate")
			}
			return err
		})
	}()
	completed := false
	t.Cleanup(func() {
		_ = lock.Rollback()
		if !completed {
			<-pruned
		}
	})
	testenv.WaitUntil(t, t.Context(), func() error {
		select {
		case err := <-pruned:
			completed = true
			if err != nil {
				t.Fatal(err)
			}
			return nil
		default:
		}
		var blocked bool
		if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%native_duplicate_challenges%')`).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			t.Fatal("challenge pruning waited for a locked row")
		}
		return errors.New("pruning has not finished")
	})
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := repository.Write(t.Context(), "org_first", corestore.Background, func(w corestore.Writer) error {
		count, err := w.(corestore.DuplicateChallengeWriter).PruneDuplicateChallenges(t.Context(), now, 1)
		if err == nil && count != 1 {
			t.Errorf("pruned %d challenges, want 1", count)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Read(t.Context(), "org_first", corestore.Serving, func(r corestore.Reader) error {
		_, err := r.(corestore.DuplicateChallengeReader).DuplicateChallenge(t.Context(), "prj_first", later.Token)
		return err
	}); err != nil {
		t.Fatalf("pruning exceeded its candidate window: %v", err)
	}
	if err := repository.Read(t.Context(), "org_second", corestore.Serving, func(r corestore.Reader) error {
		for _, tokenPart := range []string{"a", "b"} {
			if _, err := r.(corestore.DuplicateChallengeReader).DuplicateChallenge(t.Context(), "prj_first", "dup_"+strings.Repeat(tokenPart, 32)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("consumption or pruning crossed tenant boundary: %v", err)
	}
}
