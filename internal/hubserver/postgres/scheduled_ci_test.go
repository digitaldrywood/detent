package postgres_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestScheduledDecisionTenantReplay(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, repository, org)
		now := time.Now().UTC()
		input := progresscheck.CIFailure{Repository: "owner/repository", HeadSHA: "head", RunID: "run", RunAttempt: "1", JobID: "job", JobName: "test", ExitStatus: "failure"}
		want := progresscheck.CIDecision{Outcome: progresscheck.CISource, Target: "test", Reason: string(org)}
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			owner, ok := w.(policystore.ScheduledWriter)
			if !ok {
				t.Fatal("PostgreSQL scheduled decision owner is missing")
			}
			before, err := w.CollaborationBytes(t.Context())
			if err != nil {
				return err
			}
			if usage, err := owner.StoreScheduledDecision(t.Context(), "prj_first", input, want, time.Now(), math.MaxInt64); err != nil || !usage.Applied {
				t.Fatalf("new decision accounting = %+v, %v", usage, err)
			}
			other := want
			other.Reason = "replacement"
			if usage, err := owner.StoreScheduledDecision(t.Context(), "prj_first", input, other, time.Now(), math.MaxInt64); err != nil || !usage.Allowed || usage.Applied {
				t.Fatalf("replayed decision accounting = %+v, %v", usage, err)
			}
			after, err := w.CollaborationBytes(t.Context())
			if err != nil {
				return err
			}
			wantBytes := int64(72)
			if org == "org_second" {
				wantBytes = 74
			}
			if after-before != wantBytes {
				t.Fatalf("decision bytes = %d, want %d", after-before, wantBytes)
			}
			got, pending, err := owner.ReadScheduledDecision(t.Context(), "prj_first", input, "job", now)
			if err != nil {
				return err
			}
			if got != want || pending {
				t.Fatalf("decision = %+v, pending = %t, want %+v and final", got, pending, want)
			}
			if usage, err := owner.UpdateScheduledDecision(t.Context(), "prj_first", input, other, math.MaxInt64); err != nil || !usage.Applied {
				t.Fatalf("updated decision accounting = %+v, %v", usage, err)
			}
			updated, err := w.CollaborationBytes(t.Context())
			if err != nil {
				return err
			}
			wantBytes = 74
			if org == "org_second" {
				wantBytes = 75
			}
			if updated-before != wantBytes {
				t.Fatalf("updated decision bytes = %d, want %d", updated-before, wantBytes)
			}
			input.HeadSHA = "changed"
			if _, _, err := owner.ReadScheduledDecision(t.Context(), "prj_first", input, "job", now); err == nil {
				t.Fatal("changed head replay accepted")
			}
			if usage, err := owner.UpdateScheduledDecision(t.Context(), "prj_first", input, want, math.MaxInt64); !errors.Is(err, corestore.ErrNotFound) || usage.Applied {
				t.Fatalf("changed head update = %+v, %v, want not found and no usage applied", usage, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		wantProjectBytes := int64(74)
		if org == "org_second" {
			wantProjectBytes = 75
		}
		var projectBytes int64
		if err := admin.QueryRowContext(t.Context(), `SELECT collaboration_bytes FROM scheduled_ci_project_storage WHERE organization_id=$1 AND project_id='prj_first'`, org).Scan(&projectBytes); err != nil || projectBytes != wantProjectBytes {
			t.Fatalf("%s project decision bytes = %d, want %d: %v", org, projectBytes, wantProjectBytes, err)
		}
		input.HeadSHA = "head"
		err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			owner := w.(policystore.ScheduledWriter)
			for _, id := range []string{"original", "replacement"} {
				if _, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: org, ID: id, WindowStart: now.Add(-time.Hour), Metric: "api_mutations", Amount: 1}, 10); err != nil {
					return err
				}
			}
			for _, step := range []struct {
				id   string
				at   time.Time
				want bool
			}{
				{"original", now, true},
				{"replacement", now, false},
				{"replacement", now.Add(time.Minute), true},
			} {
				got, err := owner.ClaimScheduledClassification(t.Context(), "prj_first", "job", step.id, step.at, step.at.Add(time.Minute))
				if err != nil || got != step.want {
					t.Fatalf("claim %s = %t, %v; want %t", step.id, got, err, step.want)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		err = repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
			return w.PruneRequestBillingUsage(t.Context(), now, 128)
		})
		if err != nil {
			t.Fatal(err)
		}
		err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			owner := w.(policystore.ScheduledWriter)
			if released, err := owner.ReleaseScheduledClassification(t.Context(), "prj_first", "job", "original", nil); err != nil || released {
				t.Fatalf("expired owner release = %t, %v", released, err)
			}
			at := now.Add(time.Minute)
			for _, boundary := range []struct {
				at      time.Time
				pending bool
			}{{at, true}, {at.Add(time.Minute), false}} {
				_, pending, err := owner.ReadScheduledDecision(t.Context(), "prj_first", input, "job", boundary.at)
				if err != nil || pending != boundary.pending {
					t.Fatalf("pending at %s = %t, %v; want %t", boundary.at, pending, err, boundary.pending)
				}
			}
			if released, err := owner.ReleaseScheduledClassification(t.Context(), "prj_first", "job", "replacement", &at); err != nil || !released {
				t.Fatalf("live owner release after retention = %t, %v", released, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		input.HeadSHA, input.JobID = "head", "denied"
		denied := errors.New("storage allowance exhausted")
		err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			usage, err := w.(policystore.ScheduledWriter).StoreScheduledDecision(t.Context(), "prj_first", input, want, time.Now(), 0)
			if err != nil || usage.Allowed {
				t.Fatalf("decision over storage allowance = %+v, %v", usage, err)
			}
			return denied
		})
		if !errors.Is(err, denied) {
			t.Fatal(err)
		}
		err = repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			_, _, err := r.(policystore.ScheduledReader).ReadScheduledDecision(t.Context(), "prj_first", input, "job", now)
			return err
		})
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("decision over storage allowance persisted: %v", err)
		}
	}
}

func TestScheduledDecisionProjectDeletionReleasesStorage(t *testing.T) {
	t.Parallel()
	repository, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	ctx := t.Context()
	now := pgstoretest.HistoryAt()
	input := progresscheck.CIFailure{Repository: "owner/repository", HeadSHA: "head", RunID: "run", RunAttempt: "1", JobID: "job", JobName: "test", ExitStatus: "failure"}
	decision := progresscheck.CIDecision{Outcome: progresscheck.CISource, Target: "test", Reason: "assertion failed"}
	for _, org := range []tracker.OrganizationID{"org_first", "org_other"} {
		if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := pgstoretest.SeedCoreOrganization(ctx, w, org); err != nil {
				return err
			}
			if _, err := w.AdjustCollaborationBytes(ctx, 10, math.MaxInt64); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
				if _, err := w.CreateProject(ctx, corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}); err != nil {
					return err
				}
				usage, err := w.(policystore.ScheduledWriter).StoreScheduledDecision(ctx, project, input, decision, now, math.MaxInt64)
				if err != nil || !usage.Applied {
					t.Fatalf("seed decision for %s/%s: %+v, %v", org, project, usage, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	rollback := errors.New("roll back project deletion")
	for _, step := range []struct {
		name     string
		rollback bool
		want     int64
	}{{"rolled back", true, 169}, {"committed", false, 90}} {
		t.Run(step.name, func(t *testing.T) {
			err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.DeleteProject(ctx, "prj_first"); err != nil {
					return err
				}
				if step.rollback {
					return rollback
				}
				return nil
			})
			if step.rollback && !errors.Is(err, rollback) || !step.rollback && err != nil {
				t.Fatalf("delete project: %v", err)
			}
			if !step.rollback {
				err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
					return w.DeleteProject(ctx, "prj_first")
				})
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("repeat project deletion = %v, want not found", err)
				}
			}
			for _, tenant := range []struct {
				org     tracker.OrganizationID
				want    int64
				deleted bool
			}{{"org_first", step.want, !step.rollback}, {"org_other", 169, false}} {
				if err := repository.ReadTenant(ctx, tenant.org, corestore.Serving, func(r tenantstore.Reader) error {
					owner := r.(policystore.ScheduledReader)
					for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
						got, _, err := owner.ReadScheduledDecision(ctx, project, input, "job", now)
						if tenant.deleted && project == "prj_first" {
							if !errors.Is(err, corestore.ErrNotFound) {
								t.Errorf("deleted project decision: %+v, %v", got, err)
							}
						} else if err != nil || got != decision {
							t.Errorf("surviving decision for %s/%s: %+v, %v", tenant.org, project, got, err)
						}
					}
					used, err := r.CollaborationBytes(ctx)
					if err != nil || used != tenant.want {
						t.Errorf("%s collaboration bytes = %d, want %d: %v", tenant.org, used, tenant.want, err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
