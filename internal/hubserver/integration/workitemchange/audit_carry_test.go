package workitemchange_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresPriorAuditEvidence(t *testing.T) {
	t.Parallel()
	trace := &priorAuditTrace{statements: make(map[string]pgx.TraceQueryStartData)}
	hub := hubfixture.OpenPool(t, hubfixture.Config{}, func(pool *pgxpool.Config) { pool.ConnConfig.Tracer = trace })
	project := hubtest.ProjectOptions{ID: "prj_audit"}
	first := hubfixture.NewNativeWith(t, hub, "", project)
	second := hubfixture.NewNativeWith(t, hub, hub.CreateOrganization(t, "Second organization"), project)
	otherProject := hubfixture.NewNative(t, hub, first.Organization(), "Other project")

	firstDisposition := seedPriorAuditEvidence(t, first, "change_first", "The route test rejected the request.", true)
	secondDisposition := seedPriorAuditEvidence(t, second, "change_first", "The guard test rejected the request.", false)
	seedPriorAuditEvidence(t, first, "change_other", "", false)
	seedPriorAuditEvidence(t, otherProject, "change_other_project", "", false)

	repository := hubfixture.ChangeInput().Repository
	for _, tc := range []struct {
		name, change, version, repository string
		scope                             hubfixture.Native
		status                            int
		run                               int64
		disposition                       *securityaudit.Disposition
		before, next                      int64
	}{
		{"newest preceding review", "change_first", "2", repository, first, http.StatusOK, 71, nil, 0, 2},
		{"same Change and tenant", "change_first", "2", repository, first, http.StatusOK, 70, &firstDisposition, 2, 0},
		{"same identifiers in second tenant", "change_first", "2", repository, second, http.StatusOK, 70, &secondDisposition, 0, 0},
		{"bounded page of human reviews", "change_other", "2", repository, first, http.StatusOK, 0, nil, 0, 2},
		{"other Change with same repository, source and run ID", "change_other", "2", repository, first, http.StatusOK, 70, nil, 2, 0},
		{"gap in version numbering", "change_other_project", "2", repository, otherProject, http.StatusOK, 70, nil, 0, 0},
		{"other project in same tenant", "change_other_project", "2", repository, first, http.StatusNotFound, 0, nil, 0, 0},
		{"other repository", "change_first", "2", "https://github.com/example/other", first, http.StatusNotFound, 0, nil, 0, 0},
		{"preceding version uses other repository", "change_other", "3", "https://github.com/example/other", first, http.StatusNotFound, 0, nil, 0, 0},
		{"first version", "change_first", "1", repository, first, http.StatusNotFound, 0, nil, 0, 0},
		{"version belongs to other Change", "change_first", "other", repository, first, http.StatusNotFound, 0, nil, 0, 0},
		{"immediate preceding version with no audit", "change_first", "3", repository, first, http.StatusOK, 0, nil, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version := tc.change + "_v" + tc.version
			if tc.version == "other" {
				version = "change_other_v2"
			}
			query := url.Values{"change_id": {tc.change}, "version_id": {version}, "repository": {tc.repository}}
			if tc.before != 0 {
				query.Set("before_review_sequence", strconv.FormatInt(tc.before, 10))
			}
			response := tc.scope.Request(t, http.MethodGet, tc.scope.Base+"/security-audits/evidence?"+query.Encode(), nil)
			hubfixture.RequireStatus(t, response, tc.status)
			if tc.status != http.StatusOK {
				return
			}
			var got struct {
				VersionID                string                      `json:"version_id"`
				VersionNumber            int64                       `json:"version_number"`
				Landing                  bool                        `json:"landing"`
				Run                      *securityaudit.Run          `json:"run"`
				Dispositions             []securityaudit.Disposition `json:"dispositions"`
				NextBeforeReviewSequence int64                       `json:"next_before_review_sequence"`
			}
			hubfixture.Decode(t, response, &got)
			prior, err := strconv.ParseInt(tc.version, 10, 64)
			if err != nil || got.VersionNumber != prior-1 || got.VersionID != tc.change+"_v"+strconv.FormatInt(prior-1, 10) {
				t.Fatalf("unexpected preceding version %q/%d: %v", got.VersionID, got.VersionNumber, err)
			}
			if got.NextBeforeReviewSequence != tc.next {
				t.Fatalf("review cursor = %d, want %d", got.NextBeforeReviewSequence, tc.next)
			}
			if tc.run == 0 {
				if got.Run != nil || len(got.Dispositions) != 0 {
					t.Fatalf("unexpected audit in bounded page: %+v", got)
				}
				return
			}
			wantFinding := securityaudit.Finding{ID: "finding_example", Certainty: securityaudit.CertaintyUndetermined, UnresolvedFact: "Does the route reach the guard?", Path: "internal/example/handler.go", CheckCommand: "go test ./internal/example -run TestRouteGuard"}
			base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
			if tc.run == 71 {
				base, head = strings.Repeat("d", 40), strings.Repeat("e", 40)
			}
			if got.Run == nil || got.Run.ID != tc.run || got.Run.Repository != repository || got.Run.HeadSHA != head || got.Run.BaseSHA != base || got.Landing != (tc.run == 71) || !reflect.DeepEqual(got.Run.Findings, []securityaudit.Finding{wantFinding}) {
				t.Fatalf("unexpected audit run: %+v", got.Run)
			}
			if tc.disposition == nil && len(got.Dispositions) != 0 || tc.disposition != nil && !reflect.DeepEqual(got.Dispositions, []securityaudit.Disposition{*tc.disposition}) {
				t.Fatalf("wrong run or finding disposition: %+v", got.Dispositions)
			}
		})
	}
	t.Run("indexed and bounded", func(t *testing.T) {
		query := eventstore.PriorAuditQuery{Project: first.Project.ID, ChangeID: "change_first", VersionID: "change_first_v2", Repository: repository, ThroughReviewSequence: 1}
		if err := hub.Repository.ReadTenant(t.Context(), first.Organization(), corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.(eventstore.PriorAuditReader).PriorAuditEvidence(t.Context(), query)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		verifyPriorAuditPlans(t, hub, first.Organization(), trace)
	})
}

func seedPriorAuditEvidence(t *testing.T, scope hubfixture.Native, changeID, evidence string, pageable bool) securityaudit.Disposition {
	t.Helper()
	ctx := t.Context()
	org := scope.Organization()
	issue := scope.Create(t, changeID)
	content := eventstore.NewContent(scope.Hub.Config.RunDetails)
	now := time.Now().UTC().Truncate(time.Microsecond)
	marshal := func(value any) []byte {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	change, err := content.PrepareChange(ctx, org, marshal(tracker.ChangeRequest{ID: changeID, OrganizationID: org, ProjectID: scope.Project.ID, WorkItemID: issue.WorkItemID, Title: "Example", Revision: 1, CreatedAt: now, UpdatedAt: now}))
	if err != nil {
		t.Fatal(err)
	}
	var versions []eventstore.Version
	input := hubfixture.ChangeInput()
	for number := 1; number <= 3; number++ {
		input.HeadSHA = strings.Repeat(string(rune('a'+number)), 40)
		if changeID == "change_other" && number == 3 {
			input.Repository = "https://github.com/example/other"
		}
		revision := int64(number)
		if changeID == "change_other_project" && number > 1 {
			revision++
		}
		version, err := content.PrepareVersion(ctx, org, marshal(tracker.ChangeVersion{ID: changeID + "_v" + strconv.Itoa(number), ChangeID: changeID, Number: revision, CreatedAt: now, ChangeVersionInput: input}))
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	prior := versions[0]
	run := securityaudit.Run{ID: 70, ProjectID: string(scope.Project.ID), IssueID: string(issue.WorkItemID), Repository: prior.Repository, BaseSHA: prior.BaseSHA, HeadSHA: prior.HeadSHA, Verdict: securityaudit.VerdictFail,
		Findings: []securityaudit.Finding{{ID: "finding_example", Certainty: securityaudit.CertaintyUndetermined, UnresolvedFact: "Does the route reach the guard?", Path: "internal/example/handler.go", CheckCommand: "go test ./internal/example -run TestRouteGuard"}}}
	review := tracker.ChangeReview{ID: "review_example", VersionID: prior.ID, Decision: "changes_requested", CreatedAt: now, SecurityAudit: &tracker.SecurityAuditReview{VersionID: prior.ID, SessionID: run.ID, Run: run}}
	if changeID == "change_other" {
		review.SecurityAudit.VersionID = ""
	}
	old, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: org, ChangeID: changeID, Kind: "review"}, marshal(review))
	if err != nil {
		t.Fatal(err)
	}
	var ordinary []eventstore.Evidence
	if changeID == "change_other" {
		for i := range 16 {
			human := tracker.ChangeReview{ID: "review_human_" + strconv.Itoa(i), VersionID: prior.ID, Decision: "commented", CreatedAt: now}
			row, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: org, ChangeID: changeID, Kind: "review"}, marshal(human))
			if err != nil {
				t.Fatal(err)
			}
			ordinary = append(ordinary, row)
		}
	}
	var newer eventstore.Evidence
	if pageable {
		review.ID += "_landing"
		review.SecurityAudit.Landing = true
		review.SecurityAudit.SessionID++
		review.SecurityAudit.Run.ID++
		review.SecurityAudit.Run.BaseSHA, review.SecurityAudit.Run.HeadSHA = strings.Repeat("d", 40), strings.Repeat("e", 40)
		newer, err = content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: org, ChangeID: changeID, Kind: "review"}, marshal(review))
		if err != nil {
			t.Fatal(err)
		}
	}
	disposition := securityaudit.Disposition{AuditRunID: run.ID, FindingID: run.Findings[0].ID, Status: securityaudit.DispositionFalsePositive,
		ServiceIdentity: securityaudit.ServiceIdentity(string(scope.Project.ID)), RecordedAt: now, Evidence: evidence}
	ref, err := scope.Hub.Config.RunDetails.StoreDetail(ctx, string(org), marshal(disposition))
	if err != nil {
		t.Fatal(err)
	}
	err = scope.Hub.Repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateChange(ctx, change); err != nil {
			return err
		}
		for _, version := range versions {
			if _, err := w.CreateVersion(ctx, version); err != nil {
				return err
			}
		}
		current := versions[1].ID
		change.CurrentVersionID = &current
		if _, err := w.UpdateChange(ctx, change); err != nil {
			return err
		}
		stored, err := w.AppendEvidence(ctx, old)
		if err != nil {
			return err
		}
		if pageable {
			if _, err := w.AppendEvidence(ctx, newer); err != nil {
				return err
			}
		}
		for _, row := range ordinary {
			if _, err := w.AppendEvidence(ctx, row); err != nil {
				return err
			}
		}
		if evidence == "" {
			return nil
		}
		row := eventstore.AuditDisposition{Organization: org, Project: scope.Project.ID, ChangeID: changeID, ReviewSequence: stored.Sequence,
			ActorID: "example_actor", RequestKey: changeID, RequestHash: "example_hash", Reference: ref, Disposition: disposition}
		row, err = w.(eventstore.AuditDispositionWriter).CreateAuditDisposition(ctx, row)
		disposition.ID = row.Disposition.ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return disposition
}

type priorAuditTrace struct {
	mu         sync.Mutex
	statements map[string]pgx.TraceQueryStartData
}

func (trace *priorAuditTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	var name string
	switch {
	case strings.Contains(data.SQL, "FROM change_versions requested"):
		name = "version"
	case strings.Contains(data.SQL, "FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND version_id=$3 AND sequence<=$4"):
		name = "review"
	case strings.Contains(data.SQL, "FROM runner_audit_dispositions WHERE organization_id=$1 AND project_id=$2 AND change_id=$3 AND review_sequence=$4"):
		name = "disposition"
	default:
		return ctx
	}
	trace.mu.Lock()
	trace.statements[name] = pgx.TraceQueryStartData{SQL: data.SQL, Args: append([]any(nil), data.Args...)}
	trace.mu.Unlock()
	return ctx
}

func (*priorAuditTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func verifyPriorAuditPlans(t *testing.T, hub *hubfixture.Hub, org tracker.OrganizationID, trace *priorAuditTrace) {
	t.Helper()
	ctx := t.Context()
	tx, err := hub.Admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil {
			t.Error(err)
		}
	}()
	for _, statement := range []string{"SET LOCAL ROLE detent_runtime_v2", "SET LOCAL enable_seqscan=off"} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, "SELECT set_config('detent.organization_id',$1,true)", org); err != nil {
		t.Fatal(err)
	}
	for _, plan := range []struct {
		name    string
		indexes []string
	}{
		{"version", []string{"change_versions_organization_id_change_id_number_key"}},
		{"review", []string{"change_evidence_release_review_idx"}},
		{"disposition", []string{"runner_audit_dispositions_review_lookup"}},
	} {
		trace.mu.Lock()
		statement := trace.statements[plan.name]
		trace.mu.Unlock()
		if statement.SQL == "" {
			t.Fatalf("missing %s query", plan.name)
		}
		var raw string
		if err := tx.QueryRowContext(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+statement.SQL, statement.Args...).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		for _, index := range plan.indexes {
			if !strings.Contains(raw, `"Index Name": "`+index+`"`) {
				t.Errorf("%s query cannot seek using %s: %s", plan.name, index, raw)
			}
		}
	}
}
