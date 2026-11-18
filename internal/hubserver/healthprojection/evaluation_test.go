package healthprojection_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthprojection"
	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type countedDetails struct {
	*hubtest.Details
	uploads atomic.Int64
}

func (d *countedDetails) StoreDetail(ctx context.Context, org string, data []byte) (attachment.ArchiveReference, error) {
	d.uploads.Add(1)
	return d.Details.StoreDetail(ctx, org, data)
}

func TestPartialEvaluationPreservesUnexaminedFindingsAndSlackReceipts(t *testing.T) {
	t.Parallel()
	details := &countedDetails{Details: hubtest.NewDetails()}
	hub := hubfixture.Open(t, hubfixture.Config{RunDetails: details})
	const slackWebhookSecret = "slack_webhook"
	projection := healthprojection.Service{Postgres: hub.Repository, Tenants: hub.Repository, Details: details, Clock: hub.Config.Clock, SlackWebhookSecret: slackWebhookSecret}
	now := hub.Now().UTC().Truncate(time.Microsecond)
	for _, org := range []tracker.OrganizationID{hubfixture.Primary, hubfixture.Secondary} {
		f := hubfixture.NewNative(t, hub, org, "Example")
		repository := hub.Repository
		envelope, err := hub.Config.SecretKeys.Seal([]byte("https://hooks.slack.com/services/example/example/example"), accessstore.SecretAAD(org, "", slackWebhookSecret))
		if err != nil {
			t.Fatal(err)
		}
		err = repository.WriteSlack(t.Context(), org, corestore.Serving, func(w postgres.SlackWriter) error {
			if _, err := w.CreateSlackIntegration(t.Context(), postgres.CreateSlackIntegrationInput{OrganizationID: org, ChannelName: "example", Enabled: true, UpdatedAt: now}); err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: slackWebhookSecret, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: slackWebhookSecret, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
		})
		if err != nil {
			t.Fatal(err)
		}
		readDeliveries := func(at time.Time) []postgres.SlackDelivery {
			t.Helper()
			var deliveries []postgres.SlackDelivery
			err := repository.ReadSlack(t.Context(), org, corestore.Serving, func(r postgres.SlackReader) error {
				var err error
				deliveries, err = r.PendingSlackDeliveries(t.Context(), postgres.PendingSlackDeliveriesInput{OrganizationID: org, NextAttemptAt: at, Limit: 10})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			return deliveries
		}
		findings := healthsignals.Evaluate(now, healthsignals.Snapshot{Runners: []healthsignals.Runner{{ID: "runner_first", Projects: []string{string(f.Project.ID)}, CreatedAt: now.Add(-time.Hour)}, {ID: "runner_second", Projects: []string{string(f.Project.ID)}, CreatedAt: now.Add(-time.Hour)}}})
		if len(findings) != 2 {
			t.Fatalf("findings=%+v", findings)
		}

		if err := projection.Publish(t.Context(), org, now, findings, healthprojection.Scope{Complete: true}); err != nil {
			t.Fatal(err)
		}
		initial := hub.Request(t, http.MethodGet, f.Base+"/health/findings?state=all", hubfixture.AdminToken, nil)
		hubfixture.RequireStatus(t, initial, http.StatusOK)
		var page healthprojection.Page
		hubfixture.Decode(t, initial, &page)
		rejectedAt := now.Add(30 * time.Second)
		before := readDeliveries(rejectedAt)
		if len(before) != 2 || before[0].Event != "opened" || before[1].Event != "opened" {
			t.Fatalf("initial Slack receipts=%+v", before)
		}
		extra := healthsignals.NewFinding("runner_heartbeat_gap", "instance", "runner", "runner_extra", "Example heartbeat gap", "Inspect runner", []string{string(f.Project.ID)}, healthsignals.Evidence{})
		if err := projection.Publish(t.Context(), org, rejectedAt, []healthsignals.Finding{extra, findings[0], findings[0]}, healthprojection.Scope{Complete: true}); err == nil {
			t.Fatal("duplicate evaluation succeeded")
		}
		response := hub.Request(t, http.MethodGet, f.Base+"/health/findings?state=all", hubfixture.AdminToken, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var unchanged healthprojection.Page
		hubfixture.Decode(t, response, &unchanged)
		if unchanged.LastTickAt == nil || !unchanged.LastTickAt.Equal(now) || !reflect.DeepEqual(unchanged.Items, page.Items) {
			t.Fatalf("duplicate evaluation changed findings or tick: %+v", unchanged)
		}
		if after := readDeliveries(rejectedAt); !reflect.DeepEqual(after, before) {
			t.Fatalf("duplicate evaluation changed Slack receipts: %+v", after)
		}
		second := hubfixture.NewNative(t, hub, org, "Example second scope")
		third := hubfixture.NewNative(t, hub, org, "Example third scope")
		scopeItem := second.Create(t, "Example scoped finding")
		projectSecond := healthsignals.NewFinding("example_scope", "flow", "project", string(second.Project.ID), "Example second finding", "Inspect", []string{string(second.Project.ID)}, healthsignals.Evidence{})
		projectThird := healthsignals.NewFinding("example_scope", "flow", "project", string(third.Project.ID), "Example third finding", "Inspect", []string{string(third.Project.ID)}, healthsignals.Evidence{})
		runnerScope := healthsignals.NewFinding("example_scope", "instance", "runner", "runner_scope", "Example shared runner", "Inspect", []string{string(second.Project.ID), string(third.Project.ID)}, healthsignals.Evidence{})
		unknownScope := healthsignals.NewFinding("example_scope", "instance", "organization", string(org), "Example global finding", "Inspect", []string{string(second.Project.ID), string(third.Project.ID)}, healthsignals.Evidence{})
		itemScope := healthsignals.NewFinding("example_scope", "human", "work_item", string(scopeItem.WorkItemID), "Example shared work item", "Inspect", []string{string(second.Project.ID), string(third.Project.ID)}, healthsignals.Evidence{})
		scopedFindings := []healthsignals.Finding{projectSecond, projectThird, runnerScope, unknownScope, itemScope}
		for index := range scopedFindings {
			scopedFindings[index].Severity = "watch"
		}
		seed := append(slices.Clone(findings), scopedFindings...)
		if err := projection.Publish(t.Context(), org, now, seed, healthprojection.Scope{Complete: true}); err != nil {
			t.Fatal(err)
		}
		readScoped := func(fingerprint string) (postgres.HealthRecord, []postgres.HealthProjectLink) {
			t.Helper()
			var row postgres.HealthRecord
			var links []postgres.HealthProjectLink
			err := repository.ReadHealth(t.Context(), org, corestore.Serving, func(r postgres.HealthReader) error {
				var err error
				row, err = r.RecentHealthFingerprint(t.Context(), postgres.RecentHealthFingerprintInput{OrganizationID: org, Fingerprint: fingerprint, ResolvedAt: new(now.Add(-healthsignals.ReopenWindow))})
				if err != nil {
					return err
				}
				links, err = r.HealthRecordProjects(t.Context(), postgres.HealthRecordProjectsInput{OrganizationID: org, FindingID: row.ID, Limit: 10})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			return row, links
		}
		beforeRunner, beforeRunnerLinks := readScoped(runnerScope.Fingerprint)
		if len(beforeRunnerLinks) != 2 {
			t.Fatalf("global runner membership=%+v", beforeRunnerLinks)
		}
		partial := healthprojection.Scope{Projects: map[tracker.ProjectID]bool{second.Project.ID: true}}
		incompleteRunner := scopedFindings[2]
		incompleteRunner.Projects = []string{string(second.Project.ID)}
		if err := projection.Publish(t.Context(), org, now.Add(10*time.Second), []healthsignals.Finding{incompleteRunner}, partial); !errors.Is(err, corestore.ErrInvalidPage) {
			t.Fatalf("partial runner membership accepted without proof: %v", err)
		}
		afterRunner, afterRunnerLinks := readScoped(runnerScope.Fingerprint)
		if !reflect.DeepEqual(afterRunner, beforeRunner) || !reflect.DeepEqual(afterRunnerLinks, beforeRunnerLinks) {
			t.Fatalf("unexamined runner membership changed: %+v %+v", afterRunner, afterRunnerLinks)
		}
		if err := projection.Publish(t.Context(), org, now.Add(10*time.Second), []healthsignals.Finding{scopedFindings[3]}, partial); !errors.Is(err, corestore.ErrInvalidPage) {
			t.Fatalf("unknown global publication accepted partial scope: %v", err)
		}
		if err := projection.Publish(t.Context(), org, now.Add(10*time.Second), nil, healthprojection.Scope{}); !errors.Is(err, corestore.ErrInvalidPage) {
			t.Fatalf("empty scope defaulted to complete: %v", err)
		}
		if err := projection.Publish(t.Context(), org, now.Add(15*time.Second), nil, partial); err != nil {
			t.Fatal(err)
		}
		for index, finding := range scopedFindings {
			row, _ := readScoped(finding.Fingerprint)
			if (row.ResolvedAt != nil) != (index == 0) {
				t.Fatalf("partial project scope changed unexamined finding: %+v", row)
			}
		}
		afterRunner, afterRunnerLinks = readScoped(runnerScope.Fingerprint)
		if !reflect.DeepEqual(afterRunner, beforeRunner) || !reflect.DeepEqual(afterRunnerLinks, beforeRunnerLinks) {
			t.Fatalf("membership-only scope changed absent runner or its links: %+v %+v", afterRunner, afterRunnerLinks)
		}
		beforeItem, beforeItemLinks := readScoped(itemScope.Fingerprint)
		beforeFirst, beforeFirstLinks := readScoped(projectSecond.Fingerprint)
		incompleteItem := scopedFindings[4]
		incompleteItem.Projects = []string{string(second.Project.ID)}
		validFirst := scopedFindings[0]
		validFirst.Evidence.Reason = "Example evidence from rejected partial batch"
		uploadsBefore := details.uploads.Load()
		if err := projection.Publish(t.Context(), org, now.Add(17*time.Second), []healthsignals.Finding{validFirst, incompleteItem}, partial); !errors.Is(err, corestore.ErrInvalidPage) {
			t.Fatalf("partial work item membership accepted: %v", err)
		}
		uploadsAfter := details.uploads.Load()
		if uploadsAfter != uploadsBefore {
			t.Fatalf("coverage rejection stored evidence: before=%d after=%d", uploadsBefore, uploadsAfter)
		}
		afterFirst, afterFirstLinks := readScoped(projectSecond.Fingerprint)
		if !reflect.DeepEqual(afterFirst, beforeFirst) || !reflect.DeepEqual(afterFirstLinks, beforeFirstLinks) {
			t.Fatalf("rejected batch committed its first finding: %+v %+v", afterFirst, afterFirstLinks)
		}
		afterItem, afterItemLinks := readScoped(itemScope.Fingerprint)
		if !reflect.DeepEqual(afterItem, beforeItem) || !reflect.DeepEqual(afterItemLinks, beforeItemLinks) {
			t.Fatalf("rejected membership replacement changed finding: %+v %+v", afterItem, afterItemLinks)
		}
		runnerProof := healthprojection.Scope{Runners: map[string]bool{runnerScope.Subject.ID: true}}
		refreshedRunner := scopedFindings[2]
		refreshedRunner.Summary = "Example complete runner observation"
		if err := projection.Publish(t.Context(), org, now.Add(20*time.Second), []healthsignals.Finding{refreshedRunner}, runnerProof); err != nil {
			t.Fatal(err)
		}
		afterRunner, afterRunnerLinks = readScoped(runnerScope.Fingerprint)
		if afterRunner.Summary != refreshedRunner.Summary || afterRunner.ID != beforeRunner.ID || !reflect.DeepEqual(afterRunnerLinks, beforeRunnerLinks) {
			t.Fatalf("complete runner proof lost identity or membership: %+v %+v", afterRunner, afterRunnerLinks)
		}
		refreshedRunner.Projects = []string{string(second.Project.ID)}
		if err := projection.Publish(t.Context(), org, now.Add(22*time.Second), []healthsignals.Finding{refreshedRunner}, runnerProof); err != nil {
			t.Fatal(err)
		}
		afterRunner, afterRunnerLinks = readScoped(runnerScope.Fingerprint)
		if afterRunner.ID != beforeRunner.ID || afterRunner.ResolvedAt != nil || len(afterRunnerLinks) != 1 || afterRunnerLinks[0].ProjectID != second.Project.ID {
			t.Fatalf("complete runner membership shrink failed: %+v %+v", afterRunner, afterRunnerLinks)
		}
		shrunkRunnerLinks := afterRunnerLinks
		if err := projection.Publish(t.Context(), org, now.Add(25*time.Second), nil, runnerProof); err != nil {
			t.Fatal(err)
		}
		afterRunner, afterRunnerLinks = readScoped(runnerScope.Fingerprint)
		if afterRunner.ResolvedAt == nil || !afterRunner.ResolvedAt.Equal(now.Add(25*time.Second)) || !reflect.DeepEqual(afterRunnerLinks, shrunkRunnerLinks) {
			t.Fatalf("complete runner absence did not resolve its finding: %+v %+v", afterRunner, afterRunnerLinks)
		}
		completeProjects := healthprojection.Scope{Projects: map[tracker.ProjectID]bool{second.Project.ID: true, third.Project.ID: true}}
		if err := projection.Publish(t.Context(), org, now.Add(30*time.Second), nil, completeProjects); err != nil {
			t.Fatal(err)
		}
		for index, finding := range scopedFindings {
			row, _ := readScoped(finding.Fingerprint)
			wantResolved := index == 0 || index == 1 || index == 2 || index == 4
			if (row.ResolvedAt != nil) != wantResolved {
				t.Fatalf("project coverage resolved global or retained covered finding: %+v", row)
			}
		}
		if err := repository.ReadHealth(t.Context(), org, corestore.Serving, func(r postgres.HealthReader) error {
			tick, err := r.HealthTick(t.Context())
			if err == nil && !tick.Equal(now) {
				t.Fatalf("partial work advanced complete tick=%v want=%v", tick, now)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		unchanged, err = projection.Read(t.Context(), hubscope.NativeScope{Organization: org, Project: f.Project.ID}, "all", "", "", 100)
		if err != nil || !reflect.DeepEqual(unchanged.Items, page.Items) {
			t.Fatalf("partial work changed unexamined global findings: %+v err=%v", unchanged, err)
		}
		if after := readDeliveries(rejectedAt); !reflect.DeepEqual(after, before) {
			t.Fatalf("partial work changed global Slack receipts: %+v", after)
		}
	}
}
