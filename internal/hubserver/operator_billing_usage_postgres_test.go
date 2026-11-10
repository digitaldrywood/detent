package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	hubpostgres "github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type operatorBillingUsageReadRepository struct {
	tenantstore.Repository
	accountError error
	reads        int
	issueReads   int
	snapshots    int
	projectReads int
	known        *bool
}

func (r *operatorBillingUsageReadRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(operatorBillingUsageReader{Reader: reader, repository: r})
	})
}

func (r *operatorBillingUsageReadRepository) ReadTenantSnapshot(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.snapshots++
	return r.Repository.(tenantstore.SnapshotRepository).ReadTenantSnapshot(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(operatorBillingUsageReader{Reader: reader, repository: r})
	})
}

type operatorBillingUsageReader struct {
	tenantstore.Reader
	repository *operatorBillingUsageReadRepository
}

func (r operatorBillingUsageReader) BillingAccount(ctx context.Context) (billingstore.Account, error) {
	r.repository.reads++
	if r.repository.accountError != nil {
		return billingstore.Account{}, r.repository.accountError
	}
	return r.Reader.BillingAccount(ctx)
}

func (r operatorBillingUsageReader) BillingPopulation(ctx context.Context, metric string, now time.Time, connected time.Duration) (int64, bool, error) {
	if metric == "projects" {
		r.repository.projectReads++
	}
	if r.repository.known != nil && (metric == "active_members" || metric == "unremoved_runners" || metric == "projects" || metric == "retained_collaboration_events") {
		return 0, *r.repository.known, nil
	}

	owner, ok := r.Reader.(billingstore.PopulationReader)
	if !ok {
		return 0, false, ErrBillingUsageUnavailable
	}
	return owner.BillingPopulation(ctx, metric, now, connected)
}

func (r operatorBillingUsageReader) LatestEventRecordedAt(ctx context.Context) (*time.Time, error) {
	if r.repository.known != nil {
		return nil, corestore.ErrNotFound
	}
	return r.Reader.LatestEventRecordedAt(ctx)
}
func (r operatorBillingUsageReader) LatestAccessSessionCreatedAt(ctx context.Context) (*time.Time, error) {
	if r.repository.known != nil {
		return nil, corestore.ErrNotFound
	}
	return r.Reader.LatestAccessSessionCreatedAt(ctx)
}

func (r operatorBillingUsageReader) BillingIssuePopulation(ctx context.Context) (int64, int64, bool, error) {
	r.repository.issueReads++
	if r.repository.known != nil {
		return 0, 0, *r.repository.known, nil
	}
	owner, ok := r.Reader.(billingIssuePopulationReader)
	if !ok {
		return 0, 0, false, ErrBillingUsageUnavailable
	}
	return owner.BillingIssuePopulation(ctx)
}

func operatorBillingUsageService(source *Service, repository tenantstore.Repository) *Service {
	config := source.config
	config.TenantRepository = repository
	db := &database{billingRepository: repository, billingAuditStore: source.database.billingAuditStore, hostedOrganization: source.database.hostedOrganization, aiCreditMode: source.database.aiCreditMode, aiCreditCostMultiplier: source.database.aiCreditCostMultiplier}
	db.hostedRuntime.Store(source.database.hostedRuntime.Load())
	return &Service{config: config, database: db}
}

func TestOperatorBillingUsagePostgres(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	first := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.now = func() time.Time { return now }
		cfg.Conversation = &ConversationConfig{Enabled: true, Backend: newFakeCoordinatorBackend(), Workspace: t.TempDir(), ArchiveContents: &conversationContentFixture{}}
	})
	second := coordinatorBudgetPeer(t, first)
	for index, f := range []hostedSecurityFixture{first, second} {
		organization := f.service.accessOrganization()
		user := coordinatorBudgetUser(t, f, "billing-owner", "owner", false)
		tools, ctx, record := coordinatorBudgetTools(t, f, user)
		seedOperatorBillingCounters(t, f, index, now)
		scale := int64(index + 1)
		window := now.Unix() / 3600 * 3600
		from := time.Unix(window, 0).UTC()
		for number := 1; number <= 3+index*2; number++ {
			id := f.seedIssue(t, number)
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				row, err := w.WorkItem(t.Context(), f.project, id)
				if err != nil {
					return err
				}
				if number == 1 {
					for n := 0; n < index+2; n++ {
						event := eventstore.Event{OrganizationID: organization, ProjectID: f.project, WorkItemID: id, ID: fmt.Sprintf("billing_%s_%d", organization, n), Sequence: int64(100 + n), Type: "issue.edited", SchemaVersion: 1, ActorKind: "human", ActorPrincipalID: "example", RecordedAt: now.Add(time.Hour - time.Duration(index)*2*time.Hour), HistoryHidden: n == 0, PayloadObjectKey: "orgs/" + string(organization) + "/blobs/sha256/aa/" + strings.Repeat("a", 64), PayloadSHA256: strings.Repeat("a", 64), PayloadBytes: 1, PayloadContentBytes: 1}
						if _, err := w.AppendEvent(t.Context(), event); err != nil {
							return err
						}
					}
					created := now.Add(-2*time.Hour + time.Duration(index)*4*time.Hour)
					session := accessstore.Session{OrganizationID: organization, Hash: "billing_" + string(organization), Subject: "example", CreatedAt: created, ExpiresAt: created.Add(time.Hour), IdentityCreatedAt: created, IdentityExpiresAt: created.Add(time.Hour)}
					if err := w.CreateAccessSession(t.Context(), session); err != nil {
						return err
					}
				}
				row.Archived = number > 2+index
				if number == 1 {
					row.WorkflowStateName = new("Done")
				}
				_, err = w.UpdateWorkItem(t.Context(), row)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		var accountBefore billingstore.Account
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if index == 0 {
				accountBefore = billingstore.Account{OrganizationID: organization, Mode: "test", Binding: billing.Binding{OrganizationID: string(organization), AccountID: "acct_" + string(organization), CustomerID: "cus_" + string(organization)}, Status: "none", Plan: billingstore.PlanReference{ID: "pilot_free", Version: 1}}
				if err := w.SetBillingAccount(t.Context(), accountBefore); err != nil {
					return err
				}
			}
			metrics := map[string]int64{"http_requests": 3 * scale, "http_request_bytes_known": 3 * scale, "http_request_bytes": 100 * scale, "http_response_bytes": 200 * scale, "relay_bytes": 400 * scale}
			if index == 1 {
				metrics["http_request_bytes_known"]--
			}
			for metric, amount := range metrics {
				for _, period := range []time.Time{from, from.Add(-time.Hour)} {
					value := amount
					if period != from {
						value = 99999
					}
					result, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: organization, ID: fmt.Sprintf("%s-%s-%d", organization, metric, period.Unix()), WindowStart: period, Metric: metric, Amount: value}, math.MaxInt64)
					if err != nil || !result.Allowed {
						return fmt.Errorf("seed usage: allowed=%t: %w", result.Allowed, err)
					}
				}
			}
			usage := billingstore.ConversationUsage{OrganizationID: organization, ProjectID: f.project, ConversationID: record.ID, TurnID: "turn_" + string(organization), Provider: "example-provider", Model: "example-model", OccurredAt: now, Input: 100 * scale, CachedInput: 20 * scale, Output: 50 * scale, ReasoningOutput: 10 * scale, Outcome: "completed"}
			if index == 0 {
				price := billingstore.ConversationPrice{OrganizationID: organization, ID: scale, Provider: usage.Provider, Model: usage.Model, EffectiveAt: time.Unix(0, 0).UTC(), Input: 10000, CachedInput: 10000, Output: 5000}
				if err := w.CreateConversationPrice(t.Context(), price); err != nil {
					return err
				}
				usage.PriceID, usage.CostUSD, usage.CacheSavingsUSD = &price.ID, new(1.25), new(float64(0))
			}
			_, err := w.RecordConversationUsage(t.Context(), usage)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		t.Run(string(organization), func(t *testing.T) {
			call := runner.AgentToolCall{Name: "get_billing_usage", Arguments: json.RawMessage(`{}`)}
			if f.service.database.reader != nil {
				t.Fatal("PG billing fixture has a legacy reader")
			}
			value, err := tools.screenRead(ctx, record, call)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				GeneratedAt time.Time `json:"generated_at"`
				Usage       struct {
					hostedBillingReport
					ProviderOrganizationID string     `json:"provider_organization_id"`
					Members                *int64     `json:"members"`
					Projects               *int64     `json:"projects"`
					Runners                *int64     `json:"runners"`
					Events                 *int64     `json:"events"`
					LastActivity           *time.Time `json:"last_activity"`
					CostDrivers            struct {
						Requests              *int64           `json:"requests"`
						RequestBytes          *int64           `json:"request_bytes"`
						ResponseBytes         *int64           `json:"response_bytes"`
						RelayBytes            *int64           `json:"relay_bytes"`
						ArtifactRetainedBytes *int64           `json:"artifact_retained_bytes"`
						ArtifactReservedBytes *int64           `json:"artifact_reserved_bytes"`
						OperatorAI            chatUsageSummary `json:"operator_ai"`
						OperatorAICostUSD     *float64         `json:"operator_ai_cost_usd"`
						WindowEndsAt          time.Time        `json:"window_ends_at"`
					} `json:"cost_drivers"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(raw, &response); err != nil {
				t.Fatal(err)
			}
			usage := response.Usage
			if !response.GeneratedAt.Equal(now) || usage.OrganizationID != string(organization) || usage.Entitlement.OrganizationID != string(organization) || usage.ProviderOrganizationID != f.service.config.Hosted.WorkOSOrganizationID {
				t.Fatalf("billing identity or freshness changed: %s", raw)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(raw, &struct {
				Usage *map[string]json.RawMessage `json:"usage"`
			}{Usage: &wire}); err != nil {
				t.Fatal(err)
			}
			for _, supported := range []string{"members", "projects", "runners", "events", "last_activity"} {
				if _, exists := wire[supported]; !exists {
					t.Fatalf("supported quantity %q disappeared", supported)
				}
			}
			for name, value := range map[string]*int64{"members": usage.Members, "projects": usage.Projects, "runners": usage.Runners} {
				if value == nil || *value != int64(2+index) {
					t.Fatalf("supported %s count=%v, want%d: %s", name, value, 2+index, raw)
				}
			}
			if usage.Events == nil || *usage.Events != int64(index+2) || usage.LastActivity == nil || !usage.LastActivity.Equal(now.Add(time.Duration(index+1)*time.Hour)) || slices.Contains(usage.UnavailableUsage, "events") || slices.Contains(usage.UnavailableUsage, "last_activity") {
				t.Fatalf("retained events/activity missing or crossed tenant: %s", raw)
			}
			if value, known := usage.Entitlement.Usage["members"]; known && value != int64(3+index) {
				t.Fatalf("member quota lost reserved seat: %s", raw)
			}
			if value, known := usage.Entitlement.Usage["registered_runners"]; known && value != int64(1+index) {
				t.Fatalf("runner quota regained revoked/removed registration: %s", raw)
			}
			for _, retired := range []string{"database_bytes", "healthy"} {
				if _, exists := wire[retired]; exists {
					t.Fatalf("retired operational metadata %q remains", retired)
				}
			}
			var drivers map[string]json.RawMessage
			if err := json.Unmarshal(wire["cost_drivers"], &drivers); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]int64{"active_issues": int64(2 + index), "archived_issues": scale} {
				var got int64
				if err := json.Unmarshal(drivers[key], &got); err != nil || got != want {
					t.Fatalf("%s=%s, want%d: %v", key, drivers[key], want, err)
				}
			}
			for _, retired := range []string{"database_bytes", "wal_bytes"} {
				if _, exists := drivers[retired]; exists {
					t.Fatalf("local-file driver %q remains", retired)
				}
			}
			d := usage.CostDrivers
			if d.Requests == nil || *d.Requests != 3*scale || d.ResponseBytes == nil || *d.ResponseBytes != 200*scale || d.RelayBytes == nil || *d.RelayBytes != 400*scale || !d.WindowEndsAt.Equal(from.Add(time.Hour)) {
				t.Fatalf("usage windows or tenant counters changed: %s", raw)
			}
			if (d.RequestBytes == nil) != (index == 1) || index == 0 && *d.RequestBytes != 100*scale || d.ArtifactRetainedBytes != nil || d.ArtifactReservedBytes != nil {
				t.Fatalf("unknown driver became known: %s", raw)
			}
			if usage.ChatUsage.Input != 100*scale || usage.ChatUsage.Turns != 1 || d.OperatorAI != usage.ChatUsage || (d.OperatorAICostUSD == nil) != (index == 1) || index == 0 && *d.OperatorAICostUSD != 1.25 {
				t.Fatalf("AI usage, cost or uncertainty lost: %s", raw)
			}
			for _, role := range []string{"admin", "member", "viewer"} {
				u := coordinatorBudgetUser(t, f, "billing-"+role, role, false)
				deniedTools, deniedContext, deniedRecord := coordinatorBudgetTools(t, f, u)
				if _, err := deniedTools.screenRead(deniedContext, deniedRecord, call); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("%s read owner billing: %v", role, err)
				}
			}
			for _, arguments := range []string{`{"organization_id":"org_foreign"}`, `{"customer_id":"cus_foreign"}`, `{"account_id":"acct_foreign"}`} {
				if _, err := tools.screenRead(ctx, record, runner.AgentToolCall{Name: call.Name, Arguments: json.RawMessage(arguments)}); err == nil {
					t.Fatalf("caller selected billing account: %s", arguments)
				}
			}
			probe := &operatorBillingUsageReadRepository{Repository: f.service.config.TenantRepository}
			probeService := operatorBillingUsageService(f.service, probe)
			executor := hostedOperatorExecutor{service: probeService}
			billingCall := operatortool.Call{Name: operatortool.BillingUsage, Arguments: json.RawMessage(`{}`)}
			storageError := errors.New("example billing metadata failure")
			probe.accountError = storageError
			if _, err := executor.Execute(ctx, billingCall); !errors.Is(err, errBillingUnavailable) {
				t.Fatalf("public billing read failure changed: %v", err)
			}
			if _, err := probeService.operatorBillingUsage(ctx, now); !errors.Is(err, storageError) {
				t.Fatalf("typed billing owner lost primary failure: %v", err)
			}
			probe.accountError = nil
			for callIndex, known := range []bool{false, true} {
				probe.known = &known
				value, err := executor.Execute(ctx, billingCall)
				if err != nil {
					t.Fatal(err)
				}
				if probe.projectReads != callIndex+1 {
					t.Fatalf("project population reads=%d, want%d", probe.projectReads, callIndex+1)
				}
				if probe.snapshots != callIndex+1 {
					t.Fatalf("metadata snapshots=%d, want%d", probe.snapshots, callIndex+1)
				}
				if probe.issueReads != callIndex+1 {
					t.Fatalf("population scans=%d, want%d", probe.issueReads, callIndex+1)
				}
				raw, err := json.Marshal(value.Content)
				if err != nil {
					t.Fatal(err)
				}
				var result struct {
					Usage struct {
						Members      *int64     `json:"members"`
						Projects     *int64     `json:"projects"`
						Runners      *int64     `json:"runners"`
						Events       *int64     `json:"events"`
						LastActivity *time.Time `json:"last_activity"`
						Unavailable  []string   `json:"unavailable_usage"`
						CostDrivers  struct {
							ActiveIssues   *int64 `json:"active_issues"`
							ArchivedIssues *int64 `json:"archived_issues"`
						} `json:"cost_drivers"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				if result.Usage.LastActivity != nil || slices.Contains(result.Usage.Unavailable, "last_activity") {
					t.Fatalf("empty activity was unavailable or invented: %s", raw)
				}
				for name, value := range map[string]*int64{"members": result.Usage.Members, "projects": result.Usage.Projects, "runners": result.Usage.Runners, "events": result.Usage.Events} {
					if (value != nil) != known || value != nil && *value != 0 || !known && !slices.Contains(result.Usage.Unavailable, name) {
						t.Fatalf("supported counter availability changed: known=%t name=%s value=%v", known, name, value)
					}
				}
				for metric, value := range map[string]*int64{"active_issues": result.Usage.CostDrivers.ActiveIssues, "archived_issues": result.Usage.CostDrivers.ArchivedIssues} {
					if (value != nil) != known || value != nil && *value != 0 {
						t.Fatalf("population availability changed: known=%t metric=%s value=%v", known, metric, value)
					}
				}
			}
		})
		if err := f.service.database.billingRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			account, err := r.BillingAccount(t.Context())
			if index == 1 {
				if !errors.Is(err, corestore.ErrNotFound) {
					return fmt.Errorf("free tenant acquired account: %w", err)
				}
				return nil
			}
			if err == nil && !reflect.DeepEqual(account, accountBefore) {
				return errors.New("billing read changed selected account")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("metadata_population_bounds", func(t *testing.T) {
		admin := hostedPostgresAdmin(t, first.service)
		for _, f := range []hostedSecurityFixture{first, second} {
			seedOperatorBillingPopulationHistory(t, f, "sparse", 20002, &now, true, now)
		}
		if _, err := admin.ExecContext(t.Context(), "ANALYZE hosted_members, runner_identities"); err != nil {
			t.Fatal(err)
		}
		for index, f := range []hostedSecurityFixture{first, second} {
			organization := f.service.accessOrganization()
			t.Run(string(organization), func(t *testing.T) {
				var removed, active, inactive, members int64
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FILTER(WHERE removed_at IS NOT NULL),count(*) FILTER(WHERE removed_at IS NULL) FROM runner_identities WHERE organization_id=$1`, organization).Scan(&removed, &active); err != nil || removed != 20003 || active != int64(2+index) {
					t.Fatalf("runner sparse cardinality removed=%d active=%d: %v", removed, active, err)
				}
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FILTER(WHERE NOT active),count(*) FILTER(WHERE active) FROM hosted_members WHERE organization_id=$1`, organization).Scan(&inactive, &members); err != nil || inactive != 20003 {
					t.Fatalf("member sparse cardinality inactive=%d active=%d: %v", inactive, members, err)
				}
				t.Logf("metadata_population_cardinality tenant=%s removed_runners=%d unremoved_runners=%d inactive_members=%d active_members=%d", organization, removed, active, inactive, members)
				for _, test := range []struct {
					metric, relation, index, statement string
					count                              int64
					known                              bool
				}{
					{"unremoved_runners", "runner_identities", "runner_identities_active_idx", `SELECT count(*) FROM(SELECT runner_id FROM runner_identities WHERE organization_id=$1 AND removed_at IS NULL AND $2::timestamptz IS NOT NULL ORDER BY runner_id LIMIT $3) population`, int64(2 + index), true},
					{"active_members", "hosted_members", "hosted_members_pkey", `WITH candidates AS MATERIALIZED (SELECT active FROM hosted_members WHERE organization_id=$1 ORDER BY user_id LIMIT $3)
SELECT CASE WHEN count(*)=$3 THEN $3 ELSE count(*) FILTER (WHERE active) END FROM candidates WHERE $2::timestamptz IS NOT NULL`, 10001, false},
				} {
					t.Run(test.metric, func(t *testing.T) {
						if err := f.service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
							owner, ok := r.(billingstore.PopulationReader)
							if !ok {
								return ErrBillingUsageUnavailable
							}
							count, known, err := owner.BillingPopulation(t.Context(), test.metric, now, 0)
							if err == nil && (count != test.count || known != test.known) {
								return fmt.Errorf("population %s count=%d known=%t", test.metric, count, known)
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
						operatorBillingMetadataPlan(t, admin, organization, now, test.metric, test.relation, test.index, test.statement, test.count)
					})
				}
			})
		}
		for _, f := range []hostedSecurityFixture{first, second} {
			organization := f.service.accessOrganization()
			var existing int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM runner_identities WHERE organization_id=$1 AND removed_at IS NULL`, organization).Scan(&existing); err != nil {
				t.Fatal(err)
			}
			seedOperatorBillingPopulationHistory(t, f, "cap", 10001-existing, nil, false, now)
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				count, known, err := r.(billingstore.PopulationReader).BillingPopulation(t.Context(), "unremoved_runners", now, 0)
				if err == nil && (count != 10001 || known) {
					return fmt.Errorf("runner cap count=%d known=%t", count, known)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	for index, f := range []hostedSecurityFixture{first, second} {
		t.Run("population_bound/"+string(f.service.accessOrganization()), func(t *testing.T) {
			organization := f.service.accessOrganization()
			admin := hostedPostgresAdmin(t, f.service)
			otherProject := string(f.project) + "_other"
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO projects (organization_id,id,name,profile) VALUES ($1,$2,$2,'github_compatible')`, organization, otherProject); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO issues (organization_id,project_id,number,title) VALUES ($1,$2,1,'example')`, organization, otherProject); err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				project, err := w.Project(t.Context(), f.project)
				if err != nil {
					return err
				}
				project.DeletedAt = &now
				_, err = w.UpdateProject(t.Context(), project)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			usage, err := f.service.operatorBillingUsage(t.Context(), now)
			if err != nil {
				t.Fatal(err)
			}
			if usage.CostDrivers.ActiveIssues == nil || *usage.CostDrivers.ActiveIssues != int64(2+index) || usage.CostDrivers.ArchivedIssues == nil || *usage.CostDrivers.ArchivedIssues != int64(1+index) {
				t.Fatalf("retained deleted-project or native-profile criteria changed: %+v", usage.CostDrivers)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO issues (organization_id,project_id,number,title,archived) SELECT $1,$2,100000+n,'example',n%2=0 FROM generate_series(1,10002) n`, organization, f.project); err != nil {
				t.Fatal(err)
			}
			usage, err = f.service.operatorBillingUsage(t.Context(), now)
			if err != nil {
				t.Fatal(err)
			}
			if usage.CostDrivers.ActiveIssues != nil || usage.CostDrivers.ArchivedIssues != nil || !slices.Contains(usage.UnavailableUsage, "active_issues") || !slices.Contains(usage.UnavailableUsage, "archived_issues") {
				t.Fatalf("bounded partial population became authoritative: %+v", usage)
			}
			operatorBillingPopulationPlan(t, admin, organization, false)

		})
	}

	t.Run("population_scale_10000_tenants", func(t *testing.T) {
		admin := hostedPostgresAdmin(t, first.service)
		for _, statement := range []string{
			`INSERT INTO organizations (organization_id,name,local,created_at) SELECT 'org_scale_'||n,'Example '||n,false,CURRENT_TIMESTAMP FROM generate_series(1,9998) n`,
			`INSERT INTO projects (organization_id,id,name,profile) SELECT 'org_scale_'||n,'prj_scale_'||n,'Example','native' FROM generate_series(1,9998) n`,
			`INSERT INTO issues (organization_id,project_id,number,title) SELECT 'org_scale_'||n,'prj_scale_'||n,1,'Example' FROM generate_series(1,9998) n`,
		} {
			if _, err := admin.ExecContext(t.Context(), statement); err != nil {
				t.Fatal(err)
			}
		}
		for index, f := range []hostedSecurityFixture{first, second} {
			organization := f.service.accessOrganization()
			target := 100010
			existing := 3 + index*2 + 1 + 10002
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO issues (organization_id,project_id,number,title,archived) SELECT $1,$2,200000+n,'Example',n%2=0 FROM generate_series(1,$3::integer) n`, organization, f.project, target-existing); err != nil {
				t.Fatal(err)
			}
		}
		var tenants int
		var rows int64
		if err := admin.QueryRowContext(t.Context(), `SELECT count(DISTINCT organization_id),count(*) FROM issues`).Scan(&tenants, &rows); err != nil || tenants != 10000 || rows != 210018 {
			t.Fatalf("scale cardinality tenants=%d rows=%d: %v", tenants, rows, err)
		}
		for _, f := range []hostedSecurityFixture{first, second} {
			t.Run(string(f.service.accessOrganization()), func(t *testing.T) {
				usage, err := f.service.operatorBillingUsage(t.Context(), now)
				if err != nil {
					t.Fatal(err)
				}
				if usage.CostDrivers.ActiveIssues != nil || usage.CostDrivers.ArchivedIssues != nil || !slices.Contains(usage.UnavailableUsage, "active_issues") || !slices.Contains(usage.UnavailableUsage, "archived_issues") {
					t.Fatalf("large partial population became authoritative: %+v", usage)
				}
				operatorBillingPopulationPlan(t, admin, f.service.accessOrganization(), true)
			})
		}
	})
	for _, missing := range []string{"tenant", "billing", "snapshot"} {
		t.Run("missing_"+missing+"_owner", func(t *testing.T) {
			probeService := operatorBillingUsageService(first.service, first.service.config.TenantRepository)
			switch missing {
			case "tenant":
				probeService.config.TenantRepository = nil
			case "billing":
				probeService.database.billingRepository = nil
			default:
				probeService.config.TenantRepository = struct{ tenantstore.Repository }{first.service.config.TenantRepository}
			}
			if _, err := probeService.operatorBillingUsage(t.Context(), now); !errors.Is(err, ErrBillingUsageUnavailable) {
				t.Fatalf("missing typed owner error = %v", err)
			}
		})
	}
	var tenants int
	if err := hostedPostgresAdmin(t, first.service).QueryRowContext(t.Context(), `SELECT count(DISTINCT organization_id) FROM hosted_usage_windows WHERE organization_id IN ($1,$2)`, first.service.accessOrganization(), second.service.accessOrganization()).Scan(&tenants); err != nil || tenants != 2 {
		t.Fatalf("usage did not share isolated PG database: tenants=%d error=%v", tenants, err)
	}
}

func operatorBillingPopulationPlan(t *testing.T, admin *sql.DB, organization tracker.OrganizationID, bounded bool) {
	t.Helper()
	if _, err := admin.ExecContext(t.Context(), "ANALYZE issues, projects"); err != nil {
		t.Fatal(err)
	}
	var tuples float64
	var pages, visible int64
	var distinct, correlation float64
	if err := admin.QueryRowContext(t.Context(), "SELECT reltuples,relpages,relallvisible FROM pg_class WHERE oid='issues'::regclass").Scan(&tuples, &pages, &visible); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRowContext(t.Context(), "SELECT n_distinct,correlation FROM pg_stats WHERE schemaname='public' AND tablename='issues' AND attname='organization_id'").Scan(&distinct, &correlation); err != nil {
		t.Fatal(err)
	}
	t.Logf("billing_population_cardinality tenant=%s rows=%.0f pages=%d all_visible=%d tenant_distinct=%.0f correlation=%f bounded_scale=%t", organization, tuples, pages, visible, distinct, correlation, bounded)
	const statement = `WITH candidates AS MATERIALIZED (
 SELECT project_id,archived FROM issues WHERE organization_id=$1 ORDER BY id LIMIT $2
)
SELECT count(*) FILTER (WHERE p.profile='native' AND NOT i.archived),
 count(*) FILTER (WHERE p.profile='native' AND i.archived), count(*)<$2
FROM candidates i JOIN projects p ON p.organization_id=$1 AND p.id=i.project_id`
	tx, err := admin.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(t.Context(), "SET LOCAL ROLE "+hubpostgres.RuntimeRoleName); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", string(organization)); err != nil {
		t.Fatal(err)
	}
	var raw json.RawMessage
	if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement, organization, 10001).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	t.Logf("billing_issue_population_default_plan=%s", raw)
	var plans []struct{ Plan map[string]any }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("decode plan: %v", err)
	}
	limits, issueScans := 0, 0
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		if node["Node Type"] == "Limit" {
			limits++
			if node["Actual Rows"] != float64(10001) {
				t.Errorf("candidate limit rows=%v", node["Actual Rows"])
			}
		}
		if node["Relation Name"] == "issues" {
			issueScans++
			if bounded && ((node["Node Type"] != "Index Scan" && node["Node Type"] != "Index Only Scan") || !strings.Contains(fmt.Sprint(node["Index Cond"]), string(organization)) || node["Actual Rows"].(float64) > 10001 || node["Actual Loops"] != float64(1)) {
				t.Errorf("candidate scan is not a single bounded tenant index scan: %v", node)
			}
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				walk(child.(map[string]any))
			}
		}
	}
	walk(plans[0].Plan)
	if limits != 1 || issueScans != 1 {
		t.Fatalf("candidate limits=%d issue scans=%d", limits, issueScans)
	}
}

func seedOperatorBillingCounters(t *testing.T, f hostedSecurityFixture, index int, now time.Time) {
	t.Helper()
	organization := f.service.accessOrganization()
	for n := 0; n <= index; n++ {
		coordinatorBudgetUser(t, f, fmt.Sprintf("count-member-%d", n), "member", false)
	}
	inactive := coordinatorBudgetUser(t, f, "count-inactive", "member", false)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), inactive.identity.Subject)
		if err != nil {
			return err
		}
		member.Active = false
		if err := w.UpdateAccessMember(t.Context(), member); err != nil {
			return err
		}
		if err := w.CreateAccessMemberReservation(t.Context(), accessstore.MemberReservation{OrganizationID: organization, Email: "reserved_" + string(organization) + "@example.test", ExpiresAt: now.Add(time.Hour)}); err != nil {
			return err
		}
		for n := 0; n < index+2; n++ {
			id := fmt.Sprintf("prj_count_%s_%d", organization, n)
			project, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: tracker.ProjectID(id), Name: id, Profile: "github_compatible", CreatedAt: now})
			if err != nil {
				return err
			}
			if n == index+1 {
				project.DeletedAt = &now
				if _, err := w.UpdateProject(t.Context(), project); err != nil {
					return err
				}
			}
		}
		for n := 0; n < index+3; n++ {
			id := fmt.Sprintf("runner_count_%s_%d", organization, n)
			credential := accessstore.Credential{OrganizationID: organization, ID: id, Name: id, Hash: apikey.HashToken(id), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ProjectAccess: "all", Kind: "service"}
			if n == 0 {
				credential.RevokedAt = &now
			}
			if err := w.CreateAccessCredential(t.Context(), credential); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: id, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_" + id, RunnerID: id, MachineID: tracker.MachineID(id), TokenHash: apikey.HashToken("enroll_" + id), Scope: "organization", Operations: []string{runnerauth.Read, runnerauth.Claim}, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), CreatedBy: "example-owner"}); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: id, MachineID: tracker.MachineID(id), TokenID: id, EnrollmentID: "enrollment_" + id, Scope: "organization", State: "active", Operations: []string{runnerauth.Read, runnerauth.Claim}, CapacityLimit: 1, ReportedCapacity: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
				return err
			}
			if n == index+2 {
				if err := w.RemoveRunnerIdentity(t.Context(), id, now); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func seedOperatorBillingPopulationHistory(t *testing.T, f hostedSecurityFixture, prefix string, count int, removed *time.Time, members bool, now time.Time) {
	t.Helper()
	admin := hostedPostgresAdmin(t, f.service)

	for _, statement := range []string{
		`INSERT INTO api_tokens (organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at,rotated_at,revoked_at,expires_at,last_used_at,hosted_user_id,hosted_membership_id,operator_key_scope,operator_project_access,key_kind)
SELECT source.organization_id,$4||'_'||$1||'_'||lpad(n::text,8,'0'),$4||'_'||$1||'_'||lpad(n::text,8,'0'),md5($1||$4||n::text)||md5($4||$1||n::text),md5($1||$4||n::text),source.scope,source.native_only,source.created_at,source.updated_at,source.rotated_at,coalesce($5::timestamptz,$3::timestamptz),source.expires_at,source.last_used_at,source.hosted_user_id,source.hosted_membership_id,source.operator_key_scope,source.operator_project_access,source.key_kind
FROM api_tokens source CROSS JOIN generate_series(1,$2::integer)n WHERE source.organization_id=$1 AND source.id='runner_count_'||$1||'_0'`,
		`INSERT INTO runner_enrollments (organization_id,id,runner_id,machine_id,token_hash,scope,shared_machine,operations,created_at,expires_at,created_by,redeemed_at,revoked_at)
SELECT source.organization_id,$4||'_'||$1||'_'||lpad(n::text,8,'0'),$4||'_'||$1||'_'||lpad(n::text,8,'0'),source.machine_id,md5('enroll_'||$1||$4||n::text)||md5($4||$1||n::text),source.scope,source.shared_machine,source.operations,source.created_at,source.expires_at,source.created_by,source.redeemed_at,coalesce($5::timestamptz,$3::timestamptz)
FROM runner_enrollments source CROSS JOIN generate_series(1,$2::integer)n WHERE source.organization_id=$1 AND source.id='enrollment_runner_count_'||$1||'_0'`,
		`INSERT INTO runner_identities (organization_id,runner_id,machine_id,token_id,enrollment_id,scope,operations,display_name,capacity_limit,reported_capacity,os,architecture,created_at,expires_at,last_heartbeat_at,removed_at,state,revision,tags,machine_use,isolation_tier,host_services,availability_timezone,availability_hard_deadline,capacity_config_revision,capacity_config_local_limit,capacity_config_client_limit,capacity_config_runtime_limit,capacity_config_manageable,capacity_config_constraint,capacity_config_observed_at,capacity_request_revision,capacity_request_limit,capacity_request_backend)
SELECT source.organization_id,$4||'_'||$1||'_'||lpad(n::text,8,'0'),source.machine_id,$4||'_'||$1||'_'||lpad(n::text,8,'0'),$4||'_'||$1||'_'||lpad(n::text,8,'0'),source.scope,source.operations,$4||'_'||$1||'_'||lpad(n::text,8,'0'),source.capacity_limit,source.reported_capacity,source.os,source.architecture,source.created_at,source.expires_at,$3::timestamptz,$5::timestamptz,CASE WHEN $5::timestamptz IS NULL THEN source.state ELSE 'disabled' END,source.revision,source.tags,source.machine_use,source.isolation_tier,source.host_services,source.availability_timezone,source.availability_hard_deadline,source.capacity_config_revision,source.capacity_config_local_limit,source.capacity_config_client_limit,source.capacity_config_runtime_limit,source.capacity_config_manageable,source.capacity_config_constraint,source.capacity_config_observed_at,source.capacity_request_revision,source.capacity_request_limit,source.capacity_request_backend
FROM runner_identities source CROSS JOIN generate_series(1,$2::integer)n WHERE source.organization_id=$1 AND source.runner_id='runner_count_'||$1||'_0'`,
	} {
		result, err := admin.ExecContext(t.Context(), statement, string(f.service.accessOrganization()), count, now, prefix, removed)
		if err != nil {
			t.Fatal(err)
		}
		if rows, err := result.RowsAffected(); err != nil || rows != int64(count) {
			t.Fatalf("population seed rows=%d, want%d: %v", rows, count, err)
		}
	}
	if members {
		result, err := admin.ExecContext(t.Context(), `INSERT INTO hosted_members (organization_id,user_id,email,membership_id,role,active,principal_id,notify_issues,created_at,updated_at)
SELECT source.organization_id,$4||'_'||$1||'_'||lpad(n::text,8,'0'),$4||'_'||$1||'_'||n||'@example.test',$4||'_'||$1||'_'||n,source.role,source.active,$4||'_'||$1||'_'||lpad(n::text,8,'0'),source.notify_issues,$3::timestamptz,$3::timestamptz
FROM hosted_members source CROSS JOIN generate_series(1,$2::integer)n WHERE source.organization_id=$1 AND source.user_id=$1||'_count-inactive' AND $5::timestamptz IS NOT NULL`, string(f.service.accessOrganization()), count, now, prefix, removed)
		if err != nil {
			t.Fatal(err)
		}
		if rows, err := result.RowsAffected(); err != nil || rows != int64(count) {
			t.Fatalf("member seed rows=%d, want%d: %v", rows, count, err)
		}
	}

}

func operatorBillingMetadataPlan(t *testing.T, admin *sql.DB, organization tracker.OrganizationID, now time.Time, metric, relation, index, statement string, wantRows int64) {
	t.Helper()
	tx, err := admin.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(t.Context(), "SET LOCAL ROLE "+hubpostgres.RuntimeRoleName); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", string(organization)); err != nil {
		t.Fatal(err)
	}
	var raw json.RawMessage
	if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement, organization, now, 10001).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	t.Logf("billing_metadata_default_plan metric=%s tenant=%s plan=%s", metric, organization, raw)
	var plans []struct{ Plan map[string]any }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("decode metadata plan: %v", err)
	}
	scans := 0
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		if node["Relation Name"] == relation {
			scans++
			if (node["Node Type"] != "Index Scan" && node["Node Type"] != "Index Only Scan") || node["Index Name"] != index || node["Actual Rows"] != float64(wantRows) || node["Actual Loops"] != float64(1) || !strings.Contains(fmt.Sprint(node["Index Cond"]), string(organization)) {
				t.Errorf("metadata scan does not use bounded tenant index: %v", node)
			}
			if removed, ok := node["Rows Removed by Filter"].(float64); ok && removed != 0 {
				t.Errorf("metadata scan examined filtered history: %v", node)
			}
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				walk(child.(map[string]any))
			}
		}
	}
	walk(plans[0].Plan)
	if scans != 1 {
		t.Fatalf("metadata relation scans=%d, want1", scans)
	}
}
