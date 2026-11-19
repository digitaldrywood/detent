package workitemchange_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeTypedColumnsOwnEvidenceFilters(t *testing.T) {
	t.Parallel()
	logs := &typedRecoveryOverflowLog{}
	h := hubfixture.Open(t, hubfixture.Config{Logger: slog.New(logs)})
	first := hubfixture.NewNamedChange(t, h, "typed reviews")
	second := first
	second.Issue = first.Create(t, "pending review recovery")
	path := second.Base + "/work-items/" + string(second.Issue.WorkItemID) + "/changes"
	response := h.Request(t, http.MethodPost, path, second.Token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "pending-reviews"}, Title: "Pending review recovery"})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	hubfixture.Decode(t, response, &second.Change)
	second.Path = path + "/" + second.Change.ID
	for index, f := range []hubfixture.Change{first, second} {
		pending := index != 0
		t.Run(map[bool]string{false: "projected", true: "pending"}[pending], func(t *testing.T) {
			ctx := t.Context()
			org, project := f.Project.OrganizationID, f.Project.ID
			repository := h.Repository
			content := eventstore.NewContent(h.Config.RunDetails)
			old := f.Publish(t, "old", "")
			current := f.Publish(t, "current", old.ID)
			at := h.Now().UTC().Truncate(time.Microsecond)
			actor := tracker.Actor{Kind: "human", PrincipalID: "reviewer"}
			prepare := func(kind string, value any, unprojected bool) eventstore.Evidence {
				t.Helper()
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				row, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: org, ChangeID: f.Change.ID, Kind: kind}, raw)
				if err != nil {
					t.Fatal(err)
				}
				if unprojected {
					row.ReviewKind, row.ReviewLanding = "", false
					row.ReviewSessionKnown, row.ReviewSessionID, row.ReviewSessionMachine = false, 0, ""
				}
				return row
			}
			appendRows := func(rows ...eventstore.Evidence) {
				t.Helper()
				if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
					for _, row := range rows {
						if _, err := w.AppendEvidence(ctx, row); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			read := func() tracker.ChangeDetail {
				t.Helper()
				path := f.Base + "/work-items/" + string(f.Issue.WorkItemID) + "?view=recovery"
				response := h.Request(t, http.MethodGet, path, f.Token, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var recovery tracker.NativeRecovery
				hubfixture.Decode(t, response, &recovery)
				if recovery.ChangeDetail == nil {
					t.Fatal("recovery lost current change")
				}
				return *recovery.ChangeDetail
			}
			if !pending {
				typedDecision := prepare("review", tracker.ChangeReview{ID: "payload-approval", VersionID: old.ID, Decision: "approved", Actor: actor, CreatedAt: at}, false)
				typedDecision.Decision = "commented"
				appendRows(typedDecision)
				if detail := read(); detail.Summary.NativeReview == "stale" {
					t.Fatalf("payload decision overrode typed decision: %+v", detail.Summary)
				}
			}
			reviews := []tracker.ChangeReview{
				{ID: "stale", VersionID: old.ID, Decision: "approved", Actor: actor, CreatedAt: at},
				{ID: "implementation", VersionID: current.ID, Decision: "approved", Actor: actor, ImplementationReview: &gate.ValidatorResult{Verdict: "pass"}, CreatedAt: at},
				{ID: "validator", VersionID: current.ID, Decision: "changes_requested", Actor: actor, Validator: &gate.ValidatorResult{Verdict: "rework"}, CreatedAt: at},
				{ID: "source-audit", VersionID: current.ID, Decision: "changes_requested", Actor: actor, SecurityAudit: &tracker.SecurityAuditReview{Run: securityaudit.Run{ID: 1}}, CreatedAt: at},
				{ID: "landing-audit", VersionID: current.ID, Decision: "approved", Actor: actor, SecurityAudit: &tracker.SecurityAuditReview{Landing: true, Run: securityaudit.Run{ID: 2}}, CreatedAt: at},
			}
			rows := make([]eventstore.Evidence, 0, corestore.MaxPageSize+len(reviews)+3)
			for _, review := range reviews {
				rows = append(rows, prepare("review", review, pending))
			}
			check := tracker.ChangeCheck{VersionID: current.ID, Actor: actor, ReceivedAt: at, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: current.Checks[0].CheckRunID, Conclusion: "failure", CompletedAt: at}}
			unrelated := check
			unrelated.CheckRunID = "unrelated-check"
			rows = append(rows, prepare("check", check, false), prepare("check", unrelated, false))
			for i := range corestore.MaxPageSize + 1 {
				review := tracker.ChangeReview{ID: fmt.Sprintf("comment-%d", i), VersionID: current.ID, Decision: "commented", Actor: actor, CreatedAt: at}
				rows = append(rows, prepare("review", review, pending && i < 40))
			}
			appendRows(rows...)
			detail := read()
			if len(detail.Checks) != 1 || detail.Checks[0].CheckRunID != check.CheckRunID || detail.Checks[0].Conclusion != "failure" {
				t.Fatalf("recovery lost or misattributed a failing check: %+v", detail.Checks)
			}
			ids := make(map[string]bool, len(detail.Reviews))
			for _, review := range detail.Reviews {
				ids[review.ID] = true
			}
			for _, id := range []string{"implementation", "validator", "source-audit", "landing-audit"} {
				if !ids[id] {
					t.Fatalf("bounded recovery hid %s", id)
				}
			}
			if detail.Summary.Status == "reviewed" || detail.Summary.NativeReview != "stale" {
				t.Fatalf("recovery hid a refusal or prior approval: %+v", detail.Summary)
			}
			if pending {
				extra := make([]eventstore.Evidence, corestore.MaxPageSize)
				for i := range extra {
					review := tracker.ChangeReview{ID: fmt.Sprintf("unknown-prior-%d", i), VersionID: old.ID, Decision: "approved", Actor: actor, CreatedAt: at}
					extra[i] = prepare("review", review, true)
				}
				appendRows(extra[:len(extra)-1]...)
				if detail := read(); detail.Summary.NativeReview != "stale" {
					t.Fatalf("prior review boundary lost approval: %+v", detail.Summary)
				}
				appendRows(extra[len(extra)-1])
				requireOverflowRefusal := func(boundary string) {
					t.Helper()
					logs.invalidPage.Store(false)
					path := f.Base + "/work-items/" + string(f.Issue.WorkItemID) + "?view=recovery&projection=bounded"
					response := h.Request(t, http.MethodGet, path, f.Token, nil)
					if response.Code != http.StatusInternalServerError || !logs.invalidPage.Load() {
						t.Fatalf("%s: status=%d body=%s", boundary, response.Code, response.Body.String())
					}
				}
				requireOverflowRefusal("overflow of unprojected prior approvals")
				known := prepare("review", tracker.ChangeReview{ID: "known-prior", VersionID: old.ID, Decision: "approved", Actor: actor, CreatedAt: at}, false)

				appendRows(known)
				if detail := read(); detail.Summary.NativeReview != "stale" {
					t.Fatalf("known prior approval did not resolve historical classification: %+v", detail.Summary)
				}
				unknownCurrent := 0
				for _, evidence := range rows {
					if evidence.Kind == "review" && evidence.ReviewKind == "" && evidence.VersionID != nil && *evidence.VersionID == current.ID {
						unknownCurrent++
					}
				}
				currentOverflow := make([]eventstore.Evidence, 0, corestore.MaxPageSize+1-unknownCurrent)
				for i := unknownCurrent; i <= corestore.MaxPageSize; i++ {
					review := tracker.ChangeReview{ID: fmt.Sprintf("unknown-current-%d", i), VersionID: current.ID, Decision: "approved", Actor: actor, CreatedAt: at}
					evidence := prepare("review", review, true)
					currentOverflow = append(currentOverflow, evidence)

				}
				appendRows(currentOverflow...)
				requireOverflowRefusal("overflow of unprojected current reviews with known prior approval")
			}
			if !pending {
				retained := current
				retained.ID, retained.Number, retained.AttemptID = hubapi.NewID("version"), current.Number+1, "typed-attempt"
				raw, err := json.Marshal(retained)
				if err != nil {
					t.Fatal(err)
				}
				version, err := content.PrepareVersion(ctx, org, raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.CreateVersion(ctx, version)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					for _, test := range []struct {
						id   string
						want bool
					}{{retained.ID, true}, {current.ID, false}} {
						matched, err := r.ArtifactVersionAttemptMatches(ctx, project, test.id, retained.AttemptID)
						if err != nil {
							return err
						}
						if matched != test.want {
							t.Errorf("artifact source identity for %s = %t, want %t", test.id, matched, test.want)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				admin := h.Admin
				if _, err := admin.ExecContext(ctx, `UPDATE change_versions SET head_sha=$1 WHERE organization_id=$2 AND id=$3`, strings.Repeat("e", 40), org, current.ID); err == nil {
					t.Fatal("typed recovery weakened retained source immutability")
				}
			}
		})
	}
}

type typedRecoveryOverflowLog struct{ invalidPage atomic.Bool }

func (*typedRecoveryOverflowLog) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelError
}

func (l *typedRecoveryOverflowLog) Handle(_ context.Context, record slog.Record) error {
	record.Attrs(func(attribute slog.Attr) bool {
		if attribute.Key == "error" {
			if err, ok := attribute.Value.Any().(error); ok && errors.Is(err, corestore.ErrInvalidPage) {
				l.invalidPage.Store(true)
			}
			return false
		}
		return true
	})
	return nil
}

func (l *typedRecoveryOverflowLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *typedRecoveryOverflowLog) WithGroup(string) slog.Handler      { return l }
