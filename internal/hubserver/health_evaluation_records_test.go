package hubserver

import (
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresHealthEvaluationPublishesRecurrenceAndClear(t *testing.T) {
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: details})
	now := service.config.now().UTC().Truncate(time.Microsecond)
	projects := map[tracker.OrganizationID]tracker.ProjectID{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		f := newNativeFixture(t, service, org, "Example")
		projects[org] = f.project.ID
		repository := service.database.postgres
		envelope, err := service.config.SecretKeys.Seal([]byte("https://hooks.slack.com/services/example/example/example"), accessstore.SecretAAD(org, "", slackWebhookSecret))
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
		findings := evaluateHealth(now, healthSnapshot{Runners: []healthRunner{{ID: "runner_first", Projects: []string{string(f.project.ID)}, CreatedAt: now.Add(-time.Hour)}, {ID: "runner_second", Projects: []string{string(f.project.ID)}, CreatedAt: now.Add(-time.Hour)}}})
		if len(findings) != 2 {
			t.Fatalf("findings=%+v", findings)
		}
		ids := map[string]string{}
		for _, step := range []struct {
			age   time.Duration
			clear bool
		}{{0, false}, {time.Minute, false}, {2 * time.Minute, true}, {3 * time.Minute, false}, {4 * time.Minute, true}, {2 * time.Hour, false}} {
			active := findings
			if step.clear {
				active = nil
			}
			if err := service.publishHealthEvaluation(t.Context(), org, now.Add(step.age), active); err != nil {
				t.Fatal(err)
			}
			response := performHubAPIRequest(t, service, http.MethodGet, f.base+"/health/findings?state=all", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page healthFindingsPage
			decodeHubResponse(t, response, &page)
			want := 2
			if step.age == 2*time.Hour {
				want = 4
			}
			if len(page.Items) != want {
				t.Fatalf("step=%s page=%+v", step.age, page)
			}
			if page.LastTickAt == nil || !page.LastTickAt.Equal(now.Add(step.age)) {
				t.Fatalf("tick=%v want=%v", page.LastTickAt, now.Add(step.age))
			}
			open := 0
			for _, finding := range page.Items {
				if finding.ResolvedAt == nil {
					open++
				}
				if step.age == 0 {
					ids[finding.Fingerprint] = finding.ID
				}
				if step.age < 2*time.Hour && ids[finding.Fingerprint] != finding.ID {
					t.Fatal("recurrence lost finding identity")
				}
				count, present := finding.Evidence.Counts["leases"]
				if finding.Signal != "runner_heartbeat_gap" || !present || count != 0 {
					t.Fatalf("finding=%+v", finding)
				}
			}
			if step.clear && open != 0 || !step.clear && open != 2 {
				t.Fatalf("step=%s open=%d", step.age, open)
			}
			if step.age == 0 {
				rejectedAt := now.Add(30 * time.Second)
				before := readDeliveries(rejectedAt)
				if len(before) != 2 || before[0].Event != "opened" || before[1].Event != "opened" {
					t.Fatalf("initial Slack receipts=%+v", before)
				}
				extra := newHealthFinding("runner_heartbeat_gap", "instance", "runner", "runner_extra", "Example heartbeat gap", "Inspect runner", []string{string(f.project.ID)}, healthEvidence{})
				if err := service.publishHealthEvaluation(t.Context(), org, rejectedAt, []healthFinding{extra, findings[0], findings[0]}); err == nil {
					t.Fatal("duplicate evaluation succeeded")
				}
				response := performHubAPIRequest(t, service, http.MethodGet, f.base+"/health/findings?state=all", testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var unchanged healthFindingsPage
				decodeHubResponse(t, response, &unchanged)
				if unchanged.LastTickAt == nil || !unchanged.LastTickAt.Equal(now) || !reflect.DeepEqual(unchanged.Items, page.Items) {
					t.Fatalf("duplicate evaluation changed findings or tick: %+v", unchanged)
				}
				if after := readDeliveries(rejectedAt); !reflect.DeepEqual(after, before) {
					t.Fatalf("duplicate evaluation changed Slack receipts: %+v", after)
				}
				second := newNativeFixture(t, service, org, "Example second scope")
				third := newNativeFixture(t, service, org, "Example third scope")
				scopeItem := second.create(t, "Example scoped finding")
				projectSecond := newHealthFinding("example_scope", "flow", "project", string(second.project.ID), "Example second finding", "Inspect", []string{string(second.project.ID)}, healthEvidence{})
				projectThird := newHealthFinding("example_scope", "flow", "project", string(third.project.ID), "Example third finding", "Inspect", []string{string(third.project.ID)}, healthEvidence{})
				runnerScope := newHealthFinding("example_scope", "instance", "runner", "runner_scope", "Example shared runner", "Inspect", []string{string(second.project.ID), string(third.project.ID)}, healthEvidence{})
				unknownScope := newHealthFinding("example_scope", "instance", "organization", string(org), "Example global finding", "Inspect", []string{string(second.project.ID), string(third.project.ID)}, healthEvidence{})
				itemScope := newHealthFinding("example_scope", "human", "work_item", string(scopeItem.WorkItemID), "Example shared work item", "Inspect", []string{string(second.project.ID), string(third.project.ID)}, healthEvidence{})
				scopedFindings := []healthFinding{projectSecond, projectThird, runnerScope, unknownScope, itemScope}
				for index := range scopedFindings {
					scopedFindings[index].Severity = "watch"
				}
				seed := append(slices.Clone(findings), scopedFindings...)
				if err := service.publishHealthEvaluation(t.Context(), org, now, seed); err != nil {
					t.Fatal(err)
				}
				readScoped := func(fingerprint string) (postgres.HealthRecord, []postgres.HealthProjectLink) {
					t.Helper()
					var row postgres.HealthRecord
					var links []postgres.HealthProjectLink
					err := repository.ReadHealth(t.Context(), org, corestore.Serving, func(r postgres.HealthReader) error {
						var err error
						row, err = r.RecentHealthFingerprint(t.Context(), postgres.RecentHealthFingerprintInput{OrganizationID: org, Fingerprint: fingerprint, ResolvedAt: new(now.Add(-healthReopenWindow))})
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
				partial := healthEvaluationScope{projects: map[tracker.ProjectID]bool{second.project.ID: true}}
				incompleteRunner := scopedFindings[2]
				incompleteRunner.Projects = []string{string(second.project.ID)}
				if err := service.publishHealthEvaluationScope(t.Context(), org, now.Add(10*time.Second), []healthFinding{incompleteRunner}, partial); !errors.Is(err, corestore.ErrInvalidPage) {
					t.Fatalf("partial runner membership accepted without proof: %v", err)
				}
				afterRunner, afterRunnerLinks := readScoped(runnerScope.Fingerprint)
				if !reflect.DeepEqual(afterRunner, beforeRunner) || !reflect.DeepEqual(afterRunnerLinks, beforeRunnerLinks) {
					t.Fatalf("unexamined runner membership changed: %+v %+v", afterRunner, afterRunnerLinks)
				}
				if err := service.publishHealthEvaluationScope(t.Context(), org, now.Add(10*time.Second), []healthFinding{scopedFindings[3]}, partial); !errors.Is(err, corestore.ErrInvalidPage) {
					t.Fatalf("unknown global publication accepted partial scope: %v", err)
				}
				if err := service.publishHealthEvaluationScope(t.Context(), org, now.Add(10*time.Second), nil, healthEvaluationScope{}); !errors.Is(err, corestore.ErrInvalidPage) {
					t.Fatalf("empty scope defaulted to complete: %v", err)
				}
				if err := service.publishHealthEvaluationScope(t.Context(), org, now.Add(15*time.Second), nil, partial); err != nil {
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
				incompleteItem.Projects = []string{string(second.project.ID)}
				validFirst := scopedFindings[0]
				validFirst.Evidence.Reason = "Example evidence from rejected partial batch"
				details.mu.Lock()
				uploadsBefore := details.uploads
				details.mu.Unlock()
				if err := service.publishHealthEvaluationScope(t.Context(), org, now.Add(17*time.Second), []healthFinding{validFirst, incompleteItem}, partial); !errors.Is(err, corestore.ErrInvalidPage) {
					t.Fatalf("partial work item membership accepted: %v", err)
				}
				details.mu.Lock()
				uploadsAfter := details.uploads
				details.mu.Unlock()
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
				runnerProof := healthEvaluationScope{runners: map[string]bool{runnerScope.Subject.ID: true}}
				refreshedRunner := scopedFindings[2]
				refreshedRunner.Summary = "Example complete runner observation"
				if err := service.publishHealthEvaluationScope(t.Context(), org, now.Add(20*time.Second), []healthFinding{refreshedRunner}, runnerProof); err != nil {
					t.Fatal(err)
				}
				afterRunner, afterRunnerLinks = readScoped(runnerScope.Fingerprint)
				if afterRunner.Summary != refreshedRunner.Summary || afterRunner.ID != beforeRunner.ID || !reflect.DeepEqual(afterRunnerLinks, beforeRunnerLinks) {
					t.Fatalf("complete runner proof lost identity or membership: %+v %+v", afterRunner, afterRunnerLinks)
				}
				refreshedRunner.Projects = []string{string(second.project.ID)}
				if err := service.publishHealthEvaluationScope(t.Context(), org, now.Add(22*time.Second), []healthFinding{refreshedRunner}, runnerProof); err != nil {
					t.Fatal(err)
				}
				afterRunner, afterRunnerLinks = readScoped(runnerScope.Fingerprint)
				if afterRunner.ID != beforeRunner.ID || afterRunner.ResolvedAt != nil || len(afterRunnerLinks) != 1 || afterRunnerLinks[0].ProjectID != second.project.ID {
					t.Fatalf("complete runner membership shrink failed: %+v %+v", afterRunner, afterRunnerLinks)
				}
				shrunkRunnerLinks := afterRunnerLinks
				if err := service.publishHealthEvaluationScope(t.Context(), org, now.Add(25*time.Second), nil, runnerProof); err != nil {
					t.Fatal(err)
				}
				afterRunner, afterRunnerLinks = readScoped(runnerScope.Fingerprint)
				if afterRunner.ResolvedAt == nil || !afterRunner.ResolvedAt.Equal(now.Add(25*time.Second)) || !reflect.DeepEqual(afterRunnerLinks, shrunkRunnerLinks) {
					t.Fatalf("complete runner absence did not resolve its finding: %+v %+v", afterRunner, afterRunnerLinks)
				}
				completeProjects := healthEvaluationScope{projects: map[tracker.ProjectID]bool{second.project.ID: true, third.project.ID: true}}
				if err := service.publishHealthEvaluationScope(t.Context(), org, now.Add(30*time.Second), nil, completeProjects); err != nil {
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
				unchanged, err := service.readHealthFindings(t.Context(), hubscope.NativeScope{Organization: org, Project: f.project.ID}, "all", "", "", 100)
				if err != nil || !reflect.DeepEqual(unchanged.Items, page.Items) {
					t.Fatalf("partial work changed unexamined global findings: %+v err=%v", unchanged, err)
				}
				if after := readDeliveries(rejectedAt); !reflect.DeepEqual(after, before) {
					t.Fatalf("partial work changed global Slack receipts: %+v", after)
				}
			}
		}
	}
	cfg := service.config
	cfg.TenantRepository, cfg.AccessRepository, cfg.BillingRepository = nil, nil, nil
	cfg.Hosted = &HostedConfig{OrganizationID: "org_first", WorkOSOrganizationID: "provider_org_first", PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider()}
	tamperHostedBinding(t, hostedPostgresAdmin(t, service), "UPDATE hosted_tenant SET deployment='origin',public_url=$2 WHERE organization_id=$1", cfg.Hosted.OrganizationID, cfg.Hosted.PublicURL)
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		page, err := reopened.readHealthFindings(t.Context(), hubscope.NativeScope{Organization: org, Project: projects[org]}, "all", "", "", 100)
		if err != nil || len(page.Items) != 4 {
			t.Fatalf("reopened organization=%s page=%+v err=%v", org, page, err)
		}
	}
	t.Run("current wait provenance survives tenant publication", func(t *testing.T) {
		current := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
		at := current.config.now().UTC().Truncate(time.Microsecond)
		for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
			f := newNativeFixture(t, current, organization, "Example wait authority")
			linked := f.create(t, "Example question")
			contract := f.create(t, "Example required contract")
			for index, test := range []struct {
				name string
				wait healthWait
				kind string
				id   string
			}{
				{name: "linked human question", wait: healthWait{Item: string(linked.WorkItemID), Project: string(f.project.ID), Since: at.Add(-time.Hour), Active: true}, kind: "work_item", id: string(linked.WorkItemID)},
				{name: "unlinked project question", wait: healthWait{Project: string(f.project.ID), Since: at.Add(-time.Hour), Active: true}, kind: "project", id: string(f.project.ID)},
				{name: "current contract requirement", wait: healthWait{Item: string(contract.WorkItemID), Project: string(f.project.ID), Since: at, Active: true, Reason: "Complete required issue sections"}, kind: "work_item", id: string(contract.WorkItemID)},
			} {
				t.Run(string(organization)+"/"+test.name, func(t *testing.T) {
					observed := at.Add(time.Duration(2*index) * time.Second)
					finding, active := humanWaitFinding(observed, test.wait)
					if !active {
						t.Fatal("current wait was not classified")
					}
					scope := healthEvaluationScope{projects: map[tracker.ProjectID]bool{f.project.ID: true}}
					if err := current.publishHealthEvaluationScope(t.Context(), organization, observed, []healthFinding{finding}, scope); err != nil {
						t.Fatal(err)
					}
					page, err := current.readHealthFindings(t.Context(), hubscope.NativeScope{Organization: organization, Project: f.project.ID}, "open", "", "", 10)
					if err != nil || len(page.Items) != 1 {
						t.Fatalf("current tenant wait=%+v error=%v", page, err)
					}
					row := page.Items[0]
					if row.Signal != "unanswered_human_wait" || row.Subject.Kind != test.kind || row.Subject.ID != test.id || !row.OpenedAt.Equal(test.wait.Since) || !row.LastSeenAt.Equal(observed) {
						t.Fatalf("wait provenance changed: %+v want kind=%s id=%s since=%s", row, test.kind, test.id, test.wait.Since)
					}
					if err := current.database.postgres.ReadHealth(t.Context(), organization, corestore.Serving, func(reader postgres.HealthReader) error {
						links, err := reader.HealthRecordProjects(t.Context(), postgres.HealthRecordProjectsInput{OrganizationID: organization, FindingID: row.ID, Limit: 2})
						if err == nil && (len(links) != 1 || links[0].ProjectID != f.project.ID) {
							t.Fatalf("wait project scope changed: %+v", links)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if test.wait.Reason != "" && row.Evidence.Reason != "issue_contract_unsatisfied" {
						t.Fatalf("contract requirement reason changed: %+v", row.Evidence)
					}
					if err := current.publishHealthEvaluationScope(t.Context(), organization, observed.Add(time.Second), nil, scope); err != nil {
						t.Fatal(err)
					}
					cleared, err := current.readHealthFindings(t.Context(), hubscope.NativeScope{Organization: organization, Project: f.project.ID}, "open", "", "", 10)
					if err != nil || len(cleared.Items) != 0 {
						t.Fatalf("resolved wait remained current: %+v error=%v", cleared, err)
					}
				})
			}
		}
	})

	t.Run("source coverage resolves only examined fingerprints beyond unrelated history", func(t *testing.T) {
		service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
		stored, _ := projectRecordDatabases.Load(service.database.postgres)
		admin := stored.(*sql.DB)
		now := service.config.now().UTC().Truncate(time.Microsecond)
		for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
			first := newNativeFixture(t, service, organization, "Example examined health")
			second := newNativeFixture(t, service, organization, "Example unexamined health")
			known := newHealthFinding("examined_source", "flow", "project", string(first.project.ID), "Example observed condition", "Inspect", []string{string(first.project.ID)}, healthEvidence{})
			unknown := newHealthFinding("unknown_source", "flow", "project", string(first.project.ID), "Example unknown condition", "Inspect", []string{string(first.project.ID)}, healthEvidence{})
			other := newHealthFinding("other_source", "flow", "project", string(second.project.ID), "Example other project", "Inspect", []string{string(second.project.ID)}, healthEvidence{})
			if err := service.publishHealthEvaluation(t.Context(), organization, now, []healthFinding{known, unknown, other}); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO health_findings(organization_id,id,fingerprint,signal,class,subject_kind,subject_id,opened_at,last_seen_at,severity,summary,next_action,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes) SELECT $1,'unexamined_'||n,'unexamined_'||n,'other_source','flow','project',$2,$3,$3,'watch','Example unexamined history','Inspect','','',0,0 FROM generate_series(1,10001) n`, organization, second.project.ID, now); err != nil {
				t.Fatal(err)
			}
			context := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: hubmaintenance.ByteLimit}.Context(t.Context(), hubmaintenance.Tenant{Organization: string(organization), Generation: 1})
			scope := healthEvaluationScope{projects: map[tracker.ProjectID]bool{first.project.ID: true}, fingerprints: map[string]bool{known.Fingerprint: true}}
			if err := service.publishHealthEvaluationScope(context, organization, now.Add(time.Second), nil, scope); err != nil {
				t.Fatal(err)
			}
			if err := service.database.postgres.ReadHealth(t.Context(), organization, corestore.Background, func(reader postgres.HealthReader) error {
				for _, test := range []struct {
					finding  healthFinding
					resolved bool
				}{{known, true}, {unknown, false}, {other, false}} {
					row, err := reader.RecentHealthFingerprint(t.Context(), postgres.RecentHealthFingerprintInput{OrganizationID: organization, Fingerprint: test.finding.Fingerprint, ResolvedAt: new(now.Add(-healthReopenWindow))})
					if err != nil {
						return err
					}
					if (row.ResolvedAt != nil) != test.resolved {
						t.Fatalf("fingerprint scope changed unexamined condition: signal=%s row=%+v", test.finding.Signal, row)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})

}
