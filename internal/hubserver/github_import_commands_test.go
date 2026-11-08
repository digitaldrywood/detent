package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresImportStarts(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := &Service{localOrganization: "org", database: &database{now: time.Now}, config: Config{ImportBackend: gitHubLifecycleImporter{}, TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, now: time.Now}}
	seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: "org", Name: "Example", Slug: "example"})
	service.config.Hosted = &HostedConfig{OrganizationID: "org"}
	service.database.hostedOrganization = "org"
	for _, project := range []tracker.ProjectID{"first", "second"} {
		err := repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org", ID: project, Name: string(project), Profile: "native"}, States: nativeTriageStates(nativeFixtureStates())})
			if err != nil {
				return err
			}
			record.Settings.Repository = "example-org/" + string(project)
			record.Settings.Intake = "manual"
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: "org", project: project, credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
		request := startGitHubImportRequest{Mutation: tracker.Mutation{IdempotencyKey: "start"}, IssueNumber: 1}
		options := nativeCommandOptions{OperationID: projectOperationID(scope, "POST", "/imports")}
		raw, err := service.executeNativeMutation(t.Context(), scope, options, request.Mutation, request, service.startGitHubImportOperation(request))
		if err != nil {
			t.Fatal(err)
		}
		var job GitHubImport
		if err := json.Unmarshal(raw, &job); err != nil {
			t.Fatal(err)
		}
		got, err := service.readGitHubImportRecord(t.Context(), scope, job.ID)
		if err != nil || got.ID != job.ID || got.Revision != 1 {
			t.Fatalf("job=%+v err=%v", got, err)
		}
		replay, err := service.executeNativeMutation(t.Context(), scope, options, request.Mutation, request, service.startGitHubImportOperation(request))
		if err != nil || string(replay) != string(raw) {
			t.Fatalf("import replay=%s error=%v", replay, err)
		}

		for index, stage := range []string{"comments", "timeline", "dependencies", "edits", "edits", "edits", "finished"} {
			expected := job.Revision
			key := fmt.Sprintf("advance-%d", index)
			advanced, err := service.advanceGitHubImportCommand(t.Context(), scope, job.ID, expected, key)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(advanced, &job); err != nil {
				t.Fatal(err)
			}
			if job.Stage != stage || job.Revision != expected+1 {
				t.Fatalf("advanced=%+v want stage=%s", job, stage)
			}
			replay, err := service.advanceGitHubImportCommand(t.Context(), scope, job.ID, expected, key)
			if err != nil || string(replay) != string(advanced) {
				t.Fatalf("advance replay=%s err=%v", replay, err)
			}
		}
		if job.Status != "retrieved" || job.WorkItemID == "" {
			t.Fatalf("finished=%+v", job)
		}
		if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			owner, ok := w.(projectstore.GitHubOutboxWriter)
			if !ok {
				return ErrPostgresFeatureUnavailable
			}
			_, err := owner.ClaimGitHubOutbound(t.Context(), time.Now(), time.Now().Add(time.Minute), "unexpected")
			if !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("private source intake queued an external write: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		issue, err := service.readWorkItem(t.Context(), scope, job.WorkItemID)
		if err != nil {
			t.Fatal(err)
		}
		if issue.Title != "Source title" || issue.Body != "Complete source body" || issue.Provenance == nil || issue.Provenance.ExternalID != "I_source" {
			t.Fatalf("imported=%+v", issue)
		}
		records, err := service.readGitHubImportRecords(t.Context(), scope, job.ID, 0, 10)
		if err != nil || len(records) != 3 {
			t.Fatalf("records=%+v err=%v", records, err)
		}
		page, err := service.readProjectImportRecords(t.Context(), scope, operatortool.ProjectReadRequest{ImportID: job.ID, Limit: 2})
		if err != nil || len(page.Records) != 2 || page.NextCursor == "" {
			t.Fatalf("operator records=%+v err=%v", page, err)
		}
		secondPage, err := service.readProjectImportRecords(t.Context(), scope, operatortool.ProjectReadRequest{ImportID: job.ID, Limit: 2, After: page.NextCursor})
		if err != nil || len(secondPage.Records) != 1 || secondPage.NextCursor != "" {
			t.Fatalf("operator next=%+v err=%v", secondPage, err)
		}
		sourceScope := scope
		sourceScope.integrationRepository = "example-org/" + string(project)
		staleScope := sourceScope
		staleScope.githubIntakeRevision = &githubIntakeRevision{importID: job.ID, revision: int64(job.Revision) - 1}
		revokedScope := sourceScope
		revokedScope.credential = apiCredential{ID: "revoked_" + string(project), Scope: apiScopeOperator, Hash: apikey.HashToken("example-revoked-" + string(project))}
		if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: scope.organization, ID: revokedScope.credential.ID, Name: "Example revoked " + string(project), Hash: revokedScope.credential.Hash, Scope: "operator", KeyScope: "write", ProjectAccess: "all", Kind: "service", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
				return err
			}
			return w.RevokeAccessCredential(t.Context(), revokedScope.credential.ID, time.Now())
		}); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name  string
			scope nativeScope
			want  error
		}{
			{"current manual authority", sourceScope, nil},
			{"stale import revision", staleScope, corestore.ErrRevisionConflict},
			{"revoked manual authority", revokedScope, accessstore.ErrDenied},
		} {
			t.Run(string(project)+"/"+test.name, func(t *testing.T) {
				err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
					return service.authorizeAccessScope(t.Context(), r, test.scope, true)
				})
				if !errors.Is(err, test.want) {
					t.Fatalf("authority error=%v want=%v", err, test.want)
				}
			})
		}
		batch := &tracker.GitHubBatch{ID: "batch_" + string(project), Revision: 1, Status: "importing", HubOwned: true}
		row, err := service.prepareGitHubBatchRecord(t.Context(), scope, batch)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(projectstore.IntakeBatchWriter).SaveProjectIntakeBatch(t.Context(), row, 0)
		}); err != nil {
			t.Fatal(err)
		}
		batch.Revision, batch.Status = 2, "cancelled"
		row, err = service.prepareGitHubBatchRecord(t.Context(), scope, batch)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(projectstore.IntakeBatchWriter).SaveProjectIntakeBatch(t.Context(), row, 1)
		}); err != nil {
			t.Fatal(err)
		}
		backgroundScope := sourceScope
		backgroundScope.integrationOwned = true
		backgroundScope.githubIntakeRevision = &githubIntakeRevision{batchID: batch.ID, revision: 1}
		if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			return service.authorizeAccessScope(t.Context(), r, backgroundScope, true)
		}); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("cancelled batch authorization=%v", err)
		}

		if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			comments, err := r.SelectComments(t.Context(), scope.project, issue.WorkItemID, 0, false, 10)
			if err != nil {
				return err
			}
			if len(comments) != 1 {
				t.Fatalf("comments=%+v", comments)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	repair := &gitHubRepairFixture{}
	service.config.ImportBackend, service.config.ReconcileBackend = repair, repair
	service.config.now = func() time.Time { return now }
	service.config.jitter = func() float64 { return 0.5 }
	service.config.ReconcileInterval = time.Minute
	if err := service.maintainGitHubReconciliationRecords(t.Context()); !errors.Is(err, errGitHubRepairFixture) {
		t.Fatalf("unavailable repository=%v", err)
	}
	if err := service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(repair.reads) != 1 || repair.reads[0] != "example-org/second" {
		t.Fatalf("failed repository starved repair: %v", repair.reads)
	}
	for range 2 {
		now = now.Add(time.Second)
		if err := service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(repair.reads) != 2 || repair.reads[1] != "example-org/first" {
		t.Fatalf("retry did not resume source repair: %v", repair.reads)
	}
	for _, project := range []tracker.ProjectID{"first", "second"} {
		var job projectstore.GitHubImport
		if err := repository.ReadTenant(t.Context(), "org", corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			job, err = r.(projectstore.GitHubImportReader).GitHubImportForIssue(t.Context(), project, 1)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		issue, err := service.readWorkItem(t.Context(), nativeScope{organization: "org", project: project}, job.WorkItemID)
		if err != nil {
			t.Fatal(err)
		}
		if issue.Body != "Repaired source body" {
			t.Fatalf("source body=%q", issue.Body)
		}
	}
}

var errGitHubRepairFixture = errors.New("example repository unavailable")

type gitHubRepairFixture struct {
	failed bool
	reads  []string
}

func (b *gitHubRepairFixture) Reconcile(_ context.Context, request ReconcileRequest) (ReconcileSnapshot, error) {
	if request.Repository.Name == "first" && !b.failed {
		b.failed = true
		return ReconcileSnapshot{}, errGitHubRepairFixture
	}
	return ReconcileSnapshot{Repository: RepositorySource{Owner: request.Repository.Owner, Name: request.Repository.Name, NodeID: "R_" + request.Repository.Name}}, nil
}

func (b *gitHubRepairFixture) FetchImportPage(ctx context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
	b.reads = append(b.reads, request.Repository)
	page, err := (gitHubLifecycleImporter{}).FetchImportPage(ctx, request)
	if page.Issue != nil {
		page.Issue.Body = "Repaired source body"
		page.Issue.UpdatedAt = page.Issue.UpdatedAt.Add(time.Hour)
	}
	return page, err
}

type gitHubLifecycleImporter struct{}

func (gitHubLifecycleImporter) FetchImportPage(_ context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	provenance := tracker.Provenance{Provider: "github", ExternalID: "C_source", AuthorID: "source-author", CreatedAt: now, UpdatedAt: now, ObservedAt: now}
	comment := GitHubImportRecord{SourceKey: "comment:C_source", Kind: "comment", Body: "Source comment", Data: json.RawMessage(`{"body":"Source comment"}`), Provenance: provenance}
	switch request.Stage {
	case "issue":
		return GitHubImportPage{Issue: &IssueSource{NodeID: "I_source", Number: request.IssueNumber, URL: fmt.Sprintf("https://github.com/%s/issues/%d", request.Repository, request.IssueNumber), Title: "Source title", Body: "Complete source body", AuthorID: "source-author", CreatedAt: now, UpdatedAt: now}, Records: []GitHubImportRecord{{SourceKey: "issue:I_source", Kind: "issue", Data: json.RawMessage(`{"id":"I_source"}`), Provenance: tracker.Provenance{ExternalID: "I_source"}}}}, nil
	case "comments":
		return GitHubImportPage{Records: []GitHubImportRecord{comment}}, nil
	case "timeline":
		return GitHubImportPage{Records: []GitHubImportRecord{comment, {SourceKey: "event:source", Kind: "timeline", Data: json.RawMessage(`{"event":"opened"}`)}}}, nil
	case "dependencies", "edits":
		return GitHubImportPage{}, nil
	default:
		return GitHubImportPage{}, fmt.Errorf("unexpected stage %s", request.Stage)
	}
}
