package hubserver

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type hotQueryStatement struct {
	path      string
	statement tracedStatement
}

func TestHotQueryPlans(t *testing.T) {
	f := newChangeFixture(t, newPostgresNativeService(t, Config{}))
	f.publish(t, "current", "")
	other := f.create(t, "related")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	for n := range 64 {
		organization := tracker.OrganizationID(fmt.Sprintf("org_plan_%d", n))
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", Slug: fmt.Sprintf("org-plan-%d", n), CreatedAt: f.service.config.now()})
		}); err != nil {
			t.Fatal(err)
		}
	}
	noise := make([]tracker.NativeIssue, 0, 254)
	for n := 3; n <= 256; n++ {
		noise = append(noise, seedAnalyticsTestWorkItem(t, f.service, scope.organization, scope.project, fmt.Sprintf("wi_plan_%d", n), 100000+n, "Example", "Todo", f.service.config.now()))
	}
	for i := 1; i < len(noise); i++ {
		edge := corestore.Relationship{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: noise[i-1].WorkItemID, RelatedProjectID: scope.project, RelatedWorkItemID: noise[i].WorkItemID, Type: "related"}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.SetRelationship(t.Context(), edge, "add", f.service.config.now())
		}); err != nil {
			t.Fatal(err)
		}
	}
	edge := corestore.Relationship{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: f.issue.WorkItemID, RelatedProjectID: scope.project, RelatedWorkItemID: other.WorkItemID, Type: "related"}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetRelationship(t.Context(), edge, "add", f.service.config.now())
	}); err != nil {
		t.Fatal(err)
	}
	for n := range 1024 {
		f.nativeFixture.worker(t, fmt.Sprintf("unrelated-runner-%d", n))
	}
	r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim)
	r.enroll(t)
	receiptContent := eventstore.NewContent(f.service.runDetailStore())
	receiptHash, err := hubapi.CommandHash(map[string]string{"effect": "example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		now := f.service.config.now()
		for n := range 512 {
			record := eventstore.Receipt{OrganizationID: scope.organization, ActorID: fmt.Sprintf("actor_plan_%d", n%16), Operation: "POST /example", CommandKey: fmt.Sprintf("plan-%d", n), RequestHash: receiptHash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}
			record, err := receiptContent.PrepareReceipt(t.Context(), record, []byte(fmt.Sprintf(`{"plan":%d}`, n)))
			if err != nil {
				return err
			}
			if _, err := w.PutReceipt(t.Context(), record); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	installReportsBudgetTrace(t, f.service)
	admin := hostedPostgresAdmin(t, f.service)
	if _, err := admin.ExecContext(t.Context(), "ANALYZE runner_identities;ANALYZE api_tokens;ANALYZE token_grants;ANALYZE machines;ANALYZE issues;ANALYZE issue_relationships;ANALYZE native_commands;ANALYZE native_command_states"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var statements []hotQueryStatement
	activePath := "authority"
	setPath := func(path string) { mu.Lock(); activePath = path; mu.Unlock() }
	timing := &requestTiming{Trace: func(query string, args []driver.NamedValue) {
		values := make([]any, len(args))
		for i, arg := range args {
			values[i] = arg.Value
		}
		mu.Lock()
		statements = append(statements, hotQueryStatement{activePath, tracedStatement{query, values}})
		mu.Unlock()
	}}
	ctx := withRequestTiming(t.Context(), timing)
	repository, ok := f.service.config.TenantRepository.(*postgres.Repository)
	if !ok {
		t.Fatal("authority plan requires the PostgreSQL owner")
	}
	authority, capacity, err := repository.ReadRunnerAuthority(ctx, tenantauth.RequestIdentity{Organization: scope.organization, CredentialHash: apikey.HashToken(r.redemption.Credential)}, f.service.config.now)
	if err != nil || authority.RunnerID() != r.binding.RunnerID || capacity < 1 {
		t.Fatalf("runner authority lost identity/capacity: runner=%s capacity=%d error=%v", authority.RunnerID(), capacity, err)
	}
	mu.Lock()
	authorityStatements := append([]hotQueryStatement(nil), statements...)
	mu.Unlock()
	capacityReads := 0
	for _, captured := range authorityStatements {
		statement := captured.statement
		if strings.Contains(statement.query, "least(r.capacity_limit,m.capacity)") {
			capacityReads++
		}
		for _, unrelated := range []string{"collaboration_versions", "change_versions", "native_attempts", "native_comments"} {
			if strings.Contains(statement.query, unrelated) {
				t.Fatalf("runner authority hydrated unrelated state: %s", statement.query)
			}
		}
	}
	if capacityReads != 1 {
		t.Fatalf("runner authority capacity reads=%d want=1", capacityReads)
	}
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), scope.organization, apikey.HashToken(r.redemption.Credential), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("runner auth status=%d error=%v", status, err)
	}
	setPath("relationships")
	if err := f.service.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		page, err := reader.Relationships(ctx, scope.project, f.issue.WorkItemID, corestore.RelationshipCursor{}, corestore.MaxPageSize)
		if err != nil {
			return err
		}
		if len(page.Items) != 1 || page.Items[0].RelatedWorkItemID != other.WorkItemID || page.Items[0].Type != "related" {
			t.Fatalf("scoped related issues=%+v", page.Items)
		}
		setPath("receipt")
		_, err = reader.Receipt(ctx, credential.ID, "POST /example", "missing")
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("missing command receipt: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	captured := append([]hotQueryStatement(nil), statements...)
	mu.Unlock()
	if len(captured) == 0 {
		t.Fatal("no authority/relationship/receipt statements traced")
	}
	paths := map[string]int{}
	sources := map[string]bool{}
	required := map[string]string{"authority": "api_tokens", "relationships": "issue_relationships", "receipt": "native_commands"}
	for i, capture := range captured {
		statement := capture.statement
		if !reportsBudgetReadStatement(statement.query) {
			continue
		}
		paths[capture.path]++
		plan, raw := reportsExplainPostgresPlan(t, admin, scope.organization, statement, "enable_seqscan=off")
		sources[capture.path] = sources[capture.path] || slices.Contains(hotQueryPlanRelations(plan), required[capture.path])
		t.Logf("%s SQL %d: %s\nPLAN: %s", capture.path, i, statement.query, raw)
		if violations := hotQueryPlanViolations(capture.path, statement.query, plan); len(violations) > 0 {
			t.Errorf("%s: %v\nstatement: %s\nplan: %s", capture.path, violations, statement.query, raw)
		}
	}
	for _, path := range []string{"authority", "relationships", "receipt"} {
		if paths[path] == 0 || !sources[path] {
			t.Errorf("no actual %s source read captured: queries=%d required=%s", path, paths[path], required[path])
		}
	}
	t.Run("unknown query is rejected", func(t *testing.T) {
		statement := tracedStatement{"SELECT id FROM conversations WHERE organization_id=$1 LIMIT 1", []any{scope.organization}}
		plan, raw := reportsExplainPostgresPlan(t, admin, scope.organization, statement)
		if violations := hotQueryPlanViolations("authority", statement.query, plan); len(violations) == 0 {
			t.Fatalf("unrecognized query accepted: %s", raw)
		}
	})
}

func hotQueryPlanViolations(path, statement string, plan reportsBudgetPlan) []string {
	allowedByPath := map[string][]string{
		"authority":     {"api_tokens", "hosted_members", "hosted_tenant", "runner_identities", "machines", "projects", "token_grants"},
		"relationships": {"issue_dependencies", "issue_relationships", "issues", "projects", "organizations", "project_workflow_states", "collaboration_events", "native_attempts", "leases"},
		"receipt":       {"native_commands", "native_command_states"},
	}
	allowed, ok := allowedByPath[path]
	if !ok {
		return []string{"unrecognized query path: " + path}
	}
	keys := map[string][]string{
		"native_commands":       {"actor_id =", "operation =", "command_key ="},
		"native_command_states": {"actor_id =", "operation =", "command_key ="},
		"issue_relationships":   {"first_issue_id =|second_issue_id ="},
		"runner_identities":     {"runner_id =|token_id ="},
		"api_tokens":            {"id =|token_hash ="},
		"token_grants":          {"token_id ="},
		"machines":              {"id ="},
	}
	violations := []string{}
	relations := 0
	var inspect func(reportsBudgetPlan)
	inspect = func(node reportsBudgetPlan) {
		if node.Relation != "" {
			relations++
			if !slices.Contains(allowed, node.Relation) {
				violations = append(violations, "unrecognized "+path+" query relation: "+node.Relation)
			}
		}
		if expected, ok := keys[node.Relation]; ok && node.Loops > 0 {
			conditions, indexed := reportsBudgetConditions(node, node.Relation)
			if !indexed || !strings.Contains(conditions, "organization_id =") {
				violations = append(violations, fmt.Sprintf("unscoped/unindexed %s lookup: %s", node.Relation, conditions))
			}
			for _, alternatives := range expected {
				found := false
				for _, key := range strings.Split(alternatives, "|") {
					found = found || strings.Contains(conditions, key)
				}
				if !found {
					violations = append(violations, fmt.Sprintf("missing indexed lookup %s on %s: %s", alternatives, node.Relation, conditions))
				}
			}
			if visits := (node.Rows + node.Removed + node.IndexRemoved) * node.Loops; visits > 1 {
				violations = append(violations, fmt.Sprintf("point lookup of %s visited %.0f rows", node.Relation, visits))
			}
		}
		if node.SortSpace == "Disk" {
			violations = append(violations, "hot lookup spilled ordering to disk")
		}
		for _, child := range node.Plans {
			inspect(child)
		}
	}
	inspect(plan)
	normalized := strings.ToUpper(strings.Join(strings.Fields(statement), " "))
	if relations == 0 && !strings.HasPrefix(normalized, "SELECT SET_CONFIG(") {
		violations = append(violations, "unrecognized table-free query")
	}
	return violations
}

func hotQueryPlanRelations(plan reportsBudgetPlan) []string {
	relations := []string{}
	var inspect func(reportsBudgetPlan)
	inspect = func(node reportsBudgetPlan) {
		if node.Relation != "" {
			relations = append(relations, node.Relation)
		}
		for _, child := range node.Plans {
			inspect(child)
		}
	}
	inspect(plan)
	return relations
}
