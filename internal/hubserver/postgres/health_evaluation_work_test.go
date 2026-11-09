package postgres_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthEvaluationWorkPersistsFencedProgress(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	now := historyAt().Truncate(time.Microsecond)
	pending := map[tracker.OrganizationID]healthstore.Evaluation{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
			for _, id := range []tracker.ProjectID{"prj_first", "prj_second"} {
				if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: id, Name: string(id), Profile: "native"}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		ref, _, err := attachment.PrepareArchive(string(org), 1, bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatal(err)
		}
		work := healthstore.Evaluation{OrganizationID: org, ID: "evaluation", At: now, Input: ref, FindingCount: 2, BaselineCount: 2}
		if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
			for _, id := range []tracker.ProjectID{"prj_first", "prj_second"} {
				if err := w.PutHealthBaseline(t.Context(), postgres.PutHealthBaselineInput{OrganizationID: org, ProjectID: id, Signal: "queue_growth", Lane: "Todo"}); err != nil {
					return err
				}
			}
			owner, ok := w.(healthstore.EvaluationWriter)
			if !ok {
				return errors.New("evaluation writer is missing")
			}
			var err error
			work, err = owner.StartHealthEvaluation(t.Context(), work)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		original := work
		if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
			owner, ok := w.(healthstore.EvaluationWriter)
			if !ok {
				return errors.New("evaluation writer is missing")
			}
			var err error
			work, err = owner.AdvanceHealthEvaluation(t.Context(), work, healthstore.EvaluationProgress{Phase: "publish", Position: 1})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
			owner, ok := w.(healthstore.EvaluationWriter)
			if !ok {
				return errors.New("evaluation writer is missing")
			}
			_, err := owner.AdvanceHealthEvaluation(t.Context(), original, healthstore.EvaluationProgress{Phase: "publish", Position: 1})
			return err
		}); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("stale checkpoint err=%v", err)
		}
		pending[org] = work
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		work := pending[org]
		if err := repository.ReadHealth(t.Context(), org, corestore.Background, func(r postgres.HealthReader) error {
			owner, ok := r.(healthstore.EvaluationReader)
			if !ok {
				return errors.New("evaluation reader is missing")
			}
			got, err := owner.HealthEvaluation(t.Context())
			if err != nil {
				return err
			}
			if got.ID != work.ID || got.Position != 1 || got.Revision != 2 || !got.At.Equal(now) || got.Input.Key != work.Input.Key || got.FindingCount != 2 || got.BaselineCount != 2 {
				t.Fatalf("reopened evaluation=%+v", got)
			}
			projects, err := owner.HealthEvaluationProjects(t.Context(), []tracker.ProjectID{"prj_first", "prj_second"})
			if err == nil && len(projects) != 2 {
				t.Fatalf("organization=%s usable projects=%v", org, projects)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		checkReference := func(key string, want bool) {
			t.Helper()
			if err := repository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
				owner, ok := r.(interface {
					AttachmentObjectExists(context.Context, string, string) (bool, error)
				})
				if !ok {
					return errors.New("object reference reader is missing")
				}
				got, err := owner.AttachmentObjectExists(t.Context(), "", key)
				if err == nil && got != want {
					t.Fatalf("organization=%s root retained=%t want=%t", org, got, want)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		checkReference(work.Input.Key, true)
		foreignOrg := tracker.OrganizationID("org_first")
		if org == foreignOrg {
			foreignOrg = "org_second"
		}
		foreign := pending[foreignOrg]
		checkReference(foreign.Input.Key, false)
		forged := foreign
		forged.OrganizationID = org
		if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
			owner, ok := w.(healthstore.EvaluationWriter)
			if !ok {
				return errors.New("evaluation writer is missing")
			}
			_, err := owner.LockHealthEvaluation(t.Context(), forged)
			return err
		}); !errors.Is(err, corestore.ErrTenantMismatch) {
			t.Fatalf("foreign input lock err=%v", err)
		}
		advance := func(next healthstore.EvaluationProgress, want error) {
			t.Helper()
			var updated healthstore.Evaluation
			err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
				owner, ok := w.(healthstore.EvaluationWriter)
				if !ok {
					return errors.New("evaluation writer is missing")
				}
				var err error
				updated, err = owner.AdvanceHealthEvaluation(t.Context(), work, next)
				return err
			})
			if !errors.Is(err, want) {
				t.Fatalf("progress=%+v err=%v want=%v", next, err, want)
			}
			if err == nil {
				work = updated
			}
		}
		finish := func(want error) {
			t.Helper()
			err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
				owner, ok := w.(healthstore.EvaluationWriter)
				if !ok {
					return errors.New("evaluation writer is missing")
				}
				return owner.FinishHealthEvaluation(t.Context(), work)
			})
			if !errors.Is(err, want) {
				t.Fatalf("finish err=%v want=%v", err, want)
			}
		}
		advance(healthstore.EvaluationProgress{Phase: "resolve"}, corestore.ErrInvalidPage)
		advance(healthstore.EvaluationProgress{Phase: "publish", Position: 2}, nil)
		advance(healthstore.EvaluationProgress{Phase: "resolve"}, nil)
		advance(healthstore.EvaluationProgress{Phase: "resolve", FindingCursor: "finding_z"}, nil)
		advance(healthstore.EvaluationProgress{Phase: "resolve", FindingCursor: "finding_a"}, corestore.ErrRevisionConflict)
		advance(healthstore.EvaluationProgress{Phase: "baseline_clear"}, nil)
		var first healthstore.Baseline
		for range 2 {
			var rows []healthstore.Baseline
			if err := repository.ReadHealth(t.Context(), org, corestore.Background, func(r postgres.HealthReader) error {
				owner, ok := r.(healthstore.EvaluationReader)
				if !ok {
					return errors.New("evaluation reader is missing")
				}
				var err error
				rows, err = owner.HealthEvaluationBaselines(t.Context(), work.BaselineCursor, 1)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("baseline page=%v", rows)
			}
			if first.ProjectID == "" {
				first = rows[0]
			}
			advance(healthstore.EvaluationProgress{Phase: "baseline_clear", BaselineCursor: rows[0]}, nil)
		}
		advance(healthstore.EvaluationProgress{Phase: "baseline_clear", BaselineCursor: first}, corestore.ErrRevisionConflict)
		advance(healthstore.EvaluationProgress{Phase: "baseline_put"}, nil)
		finish(corestore.ErrInvalidPage)
		advance(healthstore.EvaluationProgress{Phase: "baseline_put", Position: 1}, nil)
		finish(corestore.ErrInvalidPage)
		advance(healthstore.EvaluationProgress{Phase: "baseline_put", Position: 2}, nil)
		finish(nil)
		checkReference(work.Input.Key, false)
		if err := repository.ReadHealth(t.Context(), org, corestore.Background, func(r postgres.HealthReader) error {
			owner, ok := r.(healthstore.EvaluationReader)
			if !ok {
				return errors.New("evaluation reader is missing")
			}
			if _, err := owner.HealthEvaluation(t.Context()); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("completed work err=%v", err)
			}
			tick, err := r.HealthTick(t.Context())
			if err == nil && !tick.Equal(now) {
				t.Fatalf("completed tick=%s", tick)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
			owner, ok := w.(healthstore.EvaluationWriter)
			if !ok {
				return errors.New("evaluation writer is missing")
			}
			_, err := owner.StartHealthEvaluation(t.Context(), work)
			return err
		}); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("stale input restarted: %v", err)
		}
	}
}
