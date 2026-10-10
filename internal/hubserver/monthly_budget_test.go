package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func setMonthlyBudgetFixture(t *testing.T, f nativeFixture, organization bool, p budget.MonthlyPolicy) {
	t.Helper()
	path := f.base + "/monthly-budget"
	if organization {
		path = "/api/v2/organizations/" + string(f.project.OrganizationID) + "/monthly-budget"
	}
	var current monthlyBudgetSettings
	response := performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &current)
	request := monthlyBudgetRequest{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("budget-%t-%d", organization, current.Revision)}, ExpectedRevision: current.Revision, Policy: p}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, path, testHubAdminToken, request), http.StatusOK)
}

func monthlyPolicyFixture(t *testing.T, f nativeFixture, subscription bool) policy.Descriptor {
	t.Helper()
	descriptor := hubTestPolicy()
	if subscription {
		cfg := config.Default()
		cfg.Tracker.Kind = config.TrackerHubNative
		cfg.Budget.BillingMode = config.BillingModeSubscription
		resolved, err := config.ResolvePolicy(config.Workflow{Config: cfg})
		if err != nil {
			t.Fatal(err)
		}
		descriptor = resolved
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	return descriptor
}

func TestMonthlyBudgetAdmission(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                               string
		organization, subscription, sprite bool
		policy                             budget.MonthlyPolicy
		allowed                            bool
		directBeforePolicy                 bool
	}{
		{name: "org Sprite blocks Sprite", organization: true, sprite: true, policy: budget.MonthlyPolicy{Enabled: true, SpriteMicros: new(int64)}},
		{name: "project Sprite allows local subscription", subscription: true, policy: budget.MonthlyPolicy{Enabled: true, SpriteMicros: new(int64)}, allowed: true},
		{name: "local paid API remains billable", policy: budget.MonthlyPolicy{Enabled: true, RunnerMicros: new(int64)}},
		{name: "local subscription is free", subscription: true, policy: budget.MonthlyPolicy{Enabled: true, RunnerMicros: new(int64), TotalMicros: new(int64)}, allowed: true},
		{name: "Luna does not block runner", policy: budget.MonthlyPolicy{Enabled: true, LunaMicros: new(int64)}, allowed: true},
		{name: "total constrains paid Sprite", sprite: true, policy: budget.MonthlyPolicy{Enabled: true, TotalMicros: new(int64)}},
		{name: "disabled zero remains inert", sprite: true, policy: budget.MonthlyPolicy{SpriteMicros: new(int64)}, allowed: true},
		{name: "existing direct subscription remains free", directBeforePolicy: true, subscription: true, policy: budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, RunnerMicros: new(int64), TotalMicros: new(int64)}, allowed: true},
		{name: "existing direct paid API is stopped", directBeforePolicy: true, policy: budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, RunnerMicros: new(int64)}},
		{name: "existing direct Sprite is stopped", directBeforePolicy: true, sprite: true, policy: budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, SpriteMicros: new(int64)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			descriptor := monthlyPolicyFixture(t, f, test.subscription)
			r := placementFixtureRunner(t, f, "budget-runner", "budget", test.sprite, 2, "test-model", "gpt-6.1-sol")
			setPlacementFixture(t, f, policy.Placement{Mode: "blended"}, 2)
			issue := f.create(t, "budget issue")
			var directLease tracker.Lease
			if test.directBeforePolicy {
				_, id, err := readNativeIssue(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, string(issue.WorkItemID))
				if err != nil {
					t.Fatal(err)
				}
				directLease, err = f.service.database.Claim(t.Context(), tracker.ClaimRequest{WorkItemID: id, MachineID: r.binding.MachineID, SessionID: "direct-before-budget", TTL: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
			}
			setMonthlyBudgetFixture(t, f, test.organization, test.policy)
			if test.organization {
				setMonthlyBudgetFixture(t, f, false, budget.MonthlyPolicy{Enabled: true, SpriteMicros: new(int64(1000000)), RunnerMicros: new(int64(1000000))})
			}
			request := providerClaim(r, issue, "monthly-admission")
			request.PolicyID = descriptor.ID
			status := http.StatusConflict
			if test.allowed {
				status = http.StatusOK
			}
			if test.directBeforePolicy {
				_, err := f.service.database.Renew(t.Context(), tracker.RenewRequest{LeaseID: directLease.ID, FencingToken: directLease.FencingToken, TTL: 90 * time.Second})
				if test.allowed && err != nil || !test.allowed && !errors.Is(err, tracker.ErrLeaseConflict) {
					t.Fatalf("direct renewal allowed=%t err=%v", test.allowed, err)
				}
			} else {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, request)
				requireNativeStatus(t, response, status)
			}
			var admissions int
			if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT count(*) FROM monthly_budget_admissions`).Scan(&admissions); err != nil {
				t.Fatal(err)
			}
			want := 0
			if test.allowed || test.directBeforePolicy {
				want = 1
			}
			if admissions != want {
				t.Fatalf("admissions=%d, want %d", admissions, want)
			}
		})
	}
}

func TestMonthlyBudgetDrainLifecycle(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	monthlyPolicyFixture(t, f, false)
	for _, state := range []string{"In Review", "Rework", "Merging", "Blocked", "Backlog"} {
		dispatchable := state != "Blocked" && state != "Backlog"
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,?,?,0,?,?,?)`, f.project.ID, state, state, dispatchable, formatHubTime(f.service.config.now()), formatHubTime(f.service.config.now())); err != nil {
			t.Fatal(err)
		}
	}
	r := placementFixtureRunner(t, f, "sprite-budget", "budget", true, 2, "test-model", "gpt-6.1-sol")
	scope := setPlacementFixture(t, f, policy.Placement{Mode: "blended"}, 2)
	admitted := f.create(t, "admitted")
	request := providerClaim(r, admitted, "first-admission")
	var lease tracker.NativeLease
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, request)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &lease)
	setMonthlyBudgetFixture(t, f, true, budget.MonthlyPolicy{Enabled: true, SpriteMicros: new(int64)})
	release := func() {
		t.Helper()
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
	}
	release()
	restartConfig := f.service.config
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = openTestService(t, restartConfig)
	var persistent int
	if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT count(*) FROM monthly_budget_admissions WHERE issue_id=(SELECT id FROM issues WHERE native_id=?)`, admitted.WorkItemID).Scan(&persistent); err != nil {
		t.Fatal(err)
	}
	if persistent != 1 {
		t.Fatalf("restart lost cohort: %d", persistent)
	}
	fresh := f.create(t, "new Todo")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, fresh, "fresh")), http.StatusConflict)
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE queue_entries SET priority_override=0 WHERE issue_id=(SELECT id FROM issues WHERE native_id=?)`, fresh.WorkItemID); err != nil {
		t.Fatal(err)
	}
	request = providerClaim(r, admitted, "ranked-drain")
	request.WorkItemID = ""
	request.ProviderCandidates = append(request.ProviderCandidates, providerClaim(r, fresh, "fresh-candidate").ProviderCandidates...)
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, request)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &lease)
	if lease.WorkItemID != admitted.WorkItemID {
		t.Fatal("exhausted new urgent issue displaced drain continuation")
	}
	release()
	for i := range 100 {
		f.create(t, fmt.Sprintf("closed admission %d", i))
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE queue_entries SET priority_override=0 WHERE issue_id IN (SELECT id FROM issues WHERE project_id=? AND native_id<>?)`, f.project.ID, admitted.WorkItemID); err != nil {
		t.Fatal(err)
	}
	allowed, err := monthlySpriteAllowed(t.Context(), f.service.database.db, scope, f.service.config.now())
	if err != nil || !allowed {
		t.Fatalf("ranked cohort beyond first page lost Sprite continuation: %t %v", allowed, err)
	}
	backlog := f.create(t, "untouched Backlog")
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Backlog') WHERE native_id=?`, f.project.ID, backlog.WorkItemID); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, backlog, "backlog")), http.StatusConflict)
	savedBacklog, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(backlog.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	if savedBacklog.State != "Backlog" {
		t.Fatal("budget promoted Backlog")
	}
	for _, state := range []string{"In Progress", "In Review", "Rework", "Merging"} {
		if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state=?),revision=revision+1 WHERE native_id=?`, f.project.ID, state, admitted.WorkItemID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE queue_entries SET state=? WHERE issue_id=(SELECT id FROM issues WHERE native_id=?)`, state, admitted.WorkItemID); err != nil {
			t.Fatal(err)
		}
		var id tracker.WorkItemID
		var err error
		admitted, id, err = readNativeIssue(t.Context(), f.service.database.db, scope, string(admitted.WorkItemID))
		if err != nil {
			t.Fatal(err)
		}
		retained, err := monthlyBudgetRetainsRunner(t.Context(), f.service.database.db, scope, r.binding.MachineID)
		if err != nil || !retained {
			t.Fatalf("%s lost retained Sprite workspace: %t %v", state, retained, err)
		}
		if state == "Merging" {
			retained, err := monthlyIssueAdmitted(t.Context(), f.service.database.db, id)
			if err != nil || !retained {
				t.Fatalf("merge cohort=%t err=%v", retained, err)
			}
			decision, _, err := checkMonthlyClaim(t.Context(), f.service.database.db, scope, r.binding.MachineID, id, false, f.service.config.now())
			if err != nil || !decision.Allowed {
				t.Fatalf("merge drain=%+v err=%v", decision, err)
			}
			continue
		}
		request = providerClaim(r, admitted, "continue-"+state)
		response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, request)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &lease)
		release()
	}
	for _, state := range []string{"Blocked", "Done"} {
		if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state=?) WHERE native_id=?`, f.project.ID, state, admitted.WorkItemID); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT count(*) FROM monthly_budget_admissions WHERE issue_id=(SELECT id FROM issues WHERE native_id=?)`, admitted.WorkItemID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s retained admission", state)
		}
	}
}

func TestMonthlyBudgetConcurrentAndDirectClaims(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	monthlyPolicyFixture(t, f, false)
	r := placementFixtureRunner(t, f, "concurrent-budget", "budget", false, 8, "test-model", "gpt-6.1-sol")
	setMonthlyBudgetFixture(t, f, true, budget.MonthlyPolicy{Enabled: true, RunnerMicros: new(int64)})
	issue := f.create(t, "concurrent")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	_, id, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(issue.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	outcomes := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			_, err := f.service.database.Claim(t.Context(), tracker.ClaimRequest{WorkItemID: id, MachineID: r.binding.MachineID, SessionID: fmt.Sprint("direct-", i), TTL: time.Minute})
			outcomes <- err
		})
	}
	wg.Wait()
	close(outcomes)
	for err := range outcomes {
		if !isProviderWait(err) {
			t.Fatalf("direct bypass error=%v", err)
		}
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, issue, "native-next")), http.StatusConflict)
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT count(*) FROM leases`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("exhaustion admitted %d leases", count)
	}
}

func TestMonthlyBudgetHardStopAndRecovery(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	monthlyPolicyFixture(t, f, false)
	r := placementFixtureRunner(t, f, "hard-budget", "budget", true, 2, "test-model", "gpt-6.1-sol")
	scope := setPlacementFixture(t, f, policy.Placement{Mode: "blended"}, 2)
	issue := f.create(t, "hard-stop")
	var lease tracker.NativeLease
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, issue, "hard-first"))
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &lease)
	setMonthlyBudgetFixture(t, f, true, budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, SpriteMicros: new(int64(20000))})
	renewal := func(want int) {
		t.Helper()
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), want)
	}
	renewal(http.StatusOK)
	o := costTestObservation("cpu", now.Add(-time.Hour), now)
	if _, err := recordTestCost(t, f.service.database.db, string(scope.organization), string(scope.project), o, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM monthly_budget_leases WHERE lease_id=?`, lease.ID); err != nil {
		t.Fatal(err)
	}
	renewal(http.StatusConflict)
	allowed, err := monthlySpriteAllowed(t.Context(), f.service.database.db, scope, now)
	if err != nil || allowed {
		t.Fatalf("hard-stop wake=%t err=%v", allowed, err)
	}
	current, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(issue.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	if current.State != issue.State || current.Revision != issue.Revision || current.Body != issue.Body {
		t.Fatal("hard stop changed issue evidence")
	}
	setMonthlyBudgetFixture(t, f, true, budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, SpriteMicros: new(int64(20001))})
	renewal(http.StatusOK)
	setMonthlyBudgetFixture(t, f, true, budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, SpriteMicros: new(int64(20000))})
	renewal(http.StatusConflict)
	next := now.AddDate(0, 1, 0)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	record, found, err := readLeaseByID(t.Context(), tx, lease.ID)
	if err != nil || !found {
		t.Fatalf("lease=%t err=%v", found, err)
	}
	if err := checkMonthlyLease(t.Context(), tx, record, next); err != nil {
		t.Fatalf("rollover did not recover: %v", err)
	}
}

func TestMonthlyBudgetCorrectionsAndSettingsAuthority(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	now := f.service.config.now().UTC()
	o := costTestObservation("cpu", now.Add(-time.Hour), now)
	if _, err := recordTestCost(t, f.service.database.db, string(scope.organization), string(scope.project), o, now); err != nil {
		t.Fatal(err)
	}
	setMonthlyBudgetFixture(t, f, true, budget.MonthlyPolicy{Enabled: true, SpriteMicros: new(int64(20000)), TotalMicros: new(int64(20001))})
	decision, err := checkMonthlyBudget(t.Context(), f.service.database.db, scope, budget.CostExposure{SpriteInfrastructure: true}, false, now)
	if err != nil || decision.Allowed || len(decision.Exhausted) != 1 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	o.Revision = 2
	o.AmountMicros = nil
	o.Quantity = new(1.0)
	if _, err := recordTestCost(t, f.service.database.db, string(scope.organization), string(scope.project), o, now); err != nil {
		t.Fatal(err)
	}
	decision, err = checkMonthlyBudget(t.Context(), f.service.database.db, scope, budget.CostExposure{SpriteInfrastructure: true}, false, now)
	if err != nil || !decision.Allowed {
		t.Fatalf("correction=%+v err=%v", decision, err)
	}
	request := monthlyBudgetRequest{Mutation: tracker.Mutation{IdempotencyKey: "worker-settings"}, Policy: budget.MonthlyPolicy{Enabled: true}}
	worker := f.worker(t, "budget-reader")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/monthly-budget", worker, request), http.StatusForbidden)
	request.ExpectedRevision = 0
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, "/api/v2/organizations/"+string(scope.organization)+"/monthly-budget", testHubAdminToken, request), http.StatusConflict)
	var versions int
	if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT count(*) FROM monthly_budget_policies`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("settings audit has %d versions", versions)
	}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	settingsErr := func() error {
		_, err := updateMonthlyBudgetOperation(monthlyBudgetRequest{ExpectedRevision: 1, Policy: budget.MonthlyPolicy{Enabled: true, RunnerMicros: new(int64(-1))}})(context.Background(), tx, nativeScope{organization: scope.organization}, now)
		return err
	}()
	var invalid *nativeError
	if !errors.As(settingsErr, &invalid) || invalid.Code != "invalid_request" {
		t.Fatalf("negative cap=%v", settingsErr)
	}
}

func TestMonthlyBudgetLunaQueueAndHardStop(t *testing.T) {
	t.Parallel()
	for _, hardStop := range []bool{false, true} {
		t.Run(fmt.Sprint("hard-stop-", hardStop), func(t *testing.T) {
			f := newCoordinatorFixture(t, fmt.Sprint("monthly-luna-", hardStop))
			record := f.seed(t, "budget Luna", nil)
			if !hardStop {
				setMonthlyBudgetFixture(t, f.nativeFixture, false, budget.MonthlyPolicy{Enabled: true, LunaMicros: new(int64)})
				f.history(t, &record, conversation.RoleUser, "wait for budget", conversation.DeliverySaved)
				ran, err := f.coordinator().(*conversationTurnCoordinator).pass(record.ID)
				if err != nil || ran {
					t.Fatalf("exhausted Luna started=%t err=%v", ran, err)
				}
				messages := f.messages(t, record.ID)
				if len(messages) != 1 || messages[0].Delivery != conversation.DeliverySaved {
					t.Fatalf("queued evidence=%+v", messages)
				}
				setMonthlyBudgetFixture(t, f.nativeFixture, false, budget.MonthlyPolicy{Enabled: true, LunaMicros: new(int64(1))})
				f.backend.waitStarted(t)
				f.waitAssistant(t, record.ID, conversation.DeliveryCompleted)
				return
			}
			release := make(chan struct{})
			f.backend.setRun(blockingRun(release, "partial answer"))
			f.say(t, &record, "finish when eligible")
			f.backend.waitStarted(t)
			waitUntil(t, "partial answer", func() bool {
				reply, ok := lastReply(f.messages(t, record.ID))
				return ok && reply.Text == "partial answer"
			})
			other := newNativeFixture(t, f.service, f.organization, "unaffected-Luna")
			unrelated := f.seed(t, "other project", func(r *conversationRecord) { r.ProjectID = other.project.ID })
			f.say(t, &unrelated, "unrelated turn")
			f.backend.waitStarted(t)
			setMonthlyBudgetFixture(t, f.nativeFixture, false, budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, LunaMicros: new(int64)})
			stopped := f.waitAssistant(t, record.ID, conversation.DeliveryInterrupted)
			if stopped.Text != "partial answer" {
				t.Fatal("lost partial answer")
			}
			waitUntil(t, "budget message saved", func() bool { return f.messages(t, record.ID)[0].Delivery == conversation.DeliverySaved })
			if !f.coordinator().Cancel(unrelated.ID) {
				t.Fatal("project cap stopped an unrelated active turn")
			}
			f.waitAssistant(t, unrelated.ID, conversation.DeliveryInterrupted)
			close(release)
			setMonthlyBudgetFixture(t, f.nativeFixture, false, budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, LunaMicros: new(int64(1000000))})
			f.backend.waitStarted(t)
			f.waitAssistant(t, record.ID, conversation.DeliveryCompleted)
		})
	}
}

func TestMonthlyBudgetStopsSpriteLifecycleByScope(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	otherCtx, otherCancel := context.WithCancel(t.Context())
	defer otherCancel()
	key := spriteWakeKey{organization: scope.organization}
	otherKey := spriteWakeKey{organization: "org_other"}
	f.service.spriteWakeMu.Lock()
	f.service.spriteWakes[key] = &spriteLifecyclePass{cancel: cancel, pendingState: "Todo"}
	f.service.spriteWakes[otherKey] = &spriteLifecyclePass{cancel: otherCancel, pendingState: "Todo"}
	f.service.spriteWakeMu.Unlock()
	defer func() {
		f.service.spriteWakeMu.Lock()
		delete(f.service.spriteWakes, key)
		delete(f.service.spriteWakes, otherKey)
		f.service.spriteWakeMu.Unlock()
	}()
	setMonthlyBudgetFixture(t, f, false, budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, SpriteMicros: new(int64)})
	if ctx.Err() != nil {
		t.Fatal("project budget cancelled the organization pool")
	}
	setMonthlyBudgetFixture(t, f, true, budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, SpriteMicros: new(int64)})
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("affected lifecycle was not cancelled")
	}
	if otherCtx.Err() != nil {
		t.Fatal("unrelated lifecycle was cancelled")
	}
	f.service.spriteWakeMu.Lock()
	pending := f.service.spriteWakes[key].pendingState
	f.service.spriteWakeMu.Unlock()
	if pending != "" {
		t.Fatal("stopped lifecycle retained a reprovision request")
	}
}
