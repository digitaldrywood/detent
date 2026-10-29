package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type writerReadGuardKey struct{}

type writerReadGuard struct {
	mu         sync.Mutex
	violations []string
	queries    atomic.Int64
}

var writerReadAllowlist = map[string]string{
	"recordHostedRequest": "Hosted GET request accounting updates consumption in its own write transaction.",
}

func (g *writerReadGuard) check(ctx context.Context, operation string) error {
	if marked, _ := ctx.Value(writerReadGuardKey{}).(bool); !marked {
		return nil
	}
	pcs := make([]uintptr, 40)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	var stack []string
	for {
		frame, more := frames.Next()
		if strings.Contains(frame.Function, "/hubserver.") {
			name := frame.Function[strings.LastIndex(frame.Function, ".")+1:]
			if reason := writerReadAllowlist[name]; reason != "" {
				return nil
			}
			stack = append(stack, name)
		}
		if !more {
			break
		}
	}
	message := operation + ": " + strings.Join(stack, " -> ")
	g.mu.Lock()
	g.violations = append(g.violations, message)
	g.mu.Unlock()
	return errors.New("read-only GET used the writer: " + message)
}

type guardedWriterConnector struct {
	timedConnector
	guard *writerReadGuard
}

func (c guardedWriterConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.timedConnector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &guardedWriterConn{timedConn: conn.(*timedConn), guard: c.guard}, nil
}

type guardedWriterConn struct {
	*timedConn
	guard *writerReadGuard
}

func (c *guardedWriterConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.guard.queries.Add(1)
	if err := c.guard.check(ctx, "query"); err != nil {
		return nil, err
	}
	return c.timedConn.QueryContext(ctx, query, args)
}

func (c *guardedWriterConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.guard.check(ctx, "transaction"); err != nil {
		return nil, err
	}
	return c.timedConn.BeginTx(ctx, opts)
}

func installWriterReadGuard(t *testing.T, s *Service) *writerReadGuard {
	t.Helper()
	guard := &writerReadGuard{}
	original := s.database.db
	pool := sql.OpenDB(guardedWriterConnector{timedConnector: timedConnector{dsn: sqliteWriterDSN(s.database.path, s.config.BusyTimeout, s.config.Hosted != nil), driver: original.Driver()}, guard: guard})
	pool.SetMaxOpenConns(1)
	s.database.db = pool
	if s.conversations != nil {
		s.conversations.store.db = pool
	}
	s.echo.Pre(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().Method == http.MethodGet {
				c.SetRequest(c.Request().WithContext(context.WithValue(c.Request().Context(), writerReadGuardKey{}, true)))
			}
			return next(c)
		}
	})
	t.Cleanup(func() {
		s.tokenUseWork.Wait()
		s.database.db = original
		if s.conversations != nil {
			s.conversations.store.db = original
		}
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	return guard
}

func TestReadOnlyGETsNeverAcquireWriter(t *testing.T) {
	t.Run("native", func(t *testing.T) {
		f := newNativeFixture(t, nil, "", "reader-guard")
		issue := f.create(t, "readable issue")
		approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
		runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
		runner.enroll(t)
		guard := installWriterReadGuard(t, f.service)
		for _, suffix := range []string{"", "/onboarding", "/policy", "/labels", "/integration", "/actions", "/work-items", "/health/findings", "/runner-diagnostics", "/artifact-services", "/work-items/" + string(issue.WorkItemID), "/work-items/" + string(issue.WorkItemID) + "/comments", "/work-items/" + string(issue.WorkItemID) + "/runtime", "/work-items/" + string(issue.WorkItemID) + "/history", "/work-items/" + string(issue.WorkItemID) + "/attempts", "/work-items/" + string(issue.WorkItemID) + "/changes", "/work-items/" + string(issue.WorkItemID) + "/explanation", "/work-items/" + string(issue.WorkItemID) + "/pull-requests", "/work-items/" + string(issue.WorkItemID) + "/artifacts"} {
			t.Run(suffix, func(t *testing.T) {
				r := performHubAPIRequest(t, f.service, http.MethodGet, f.base+suffix, f.token, nil)
				if r.Code != http.StatusOK {
					t.Errorf("GET %s returned %d: %s", suffix, r.Code, r.Body.String())
				}
			})
		}
		for _, suffix := range []string{"/routing", "/capacity", "/update"} {
			t.Run("runner"+suffix, func(t *testing.T) {
				response := performHubAPIRequest(t, f.service, http.MethodGet, runner.identityPath()+suffix, testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
			})
		}
		f.service.tokenUseWork.Wait()
		guard.mu.Lock()
		defer guard.mu.Unlock()
		if len(guard.violations) > 0 {
			t.Fatalf("writer reads: %s", strings.Join(guard.violations, "\n"))
		}
	})
	t.Run("hosted", func(t *testing.T) {
		f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false, func(cfg *Config) {
			cfg.Hosted.EntitlementAdminToken = []byte(strings.Repeat("e", 32))
			cfg.Hosted.EntitlementAdministrator = "test-operator"
		})
		guard := installWriterReadGuard(t, f.service)
		for _, path := range []string{
			"/projects", "/project-rank", "/members", "/fleet", "/plan", "/billing", "/secrets/" + flySpritesToken,
			"/monthly-budget", "/model-selection", "/integrations/slack",
			"/sprite-pool", "/work-view-preference", "/work-views",
			"/work-board?project=" + f.project + "&state=" + url.QueryEscape(`{"view":"board","tab":"active","sort":"default","completedWindow":"24h"}`),
			"/issue-notification-preference", "/daily-summary-preference",
			"/projects/" + f.project + "/work-view-preference",
			"/activity?project=" + f.project,
			"/reports?project=" + f.project + "&range=24h",
			"/usage?project=" + f.project,
			"/diagnostics?project=" + f.project,
			"/diagnostics/requests",
		} {
			t.Run(path, func(t *testing.T) {
				response := f.page(t, "owner", browserHostedOrganizationBase+path)
				requireNativeStatus(t, response, http.StatusOK)
			})
		}
		for _, path := range []string{"/entitlements", "/entitlements/runtime"} {
			t.Run(path, func(t *testing.T) {
				response := performHubAPIRequest(t, f.service, http.MethodGet, browserHostedOrganizationBase+path, strings.Repeat("e", 32), nil)
				requireNativeStatus(t, response, http.StatusOK)
			})
		}
		f.service.tokenUseWork.Wait()
		guard.mu.Lock()
		defer guard.mu.Unlock()
		if len(guard.violations) > 0 {
			t.Fatalf("writer reads: %s", strings.Join(guard.violations, "\n"))
		}
	})
}

func TestWriterReadGuardDetectsQueriesAndTransactions(t *testing.T) {
	f := newNativeFixture(t, nil, "", "guard-negative")
	guard := installWriterReadGuard(t, f.service)
	ctx := context.WithValue(t.Context(), writerReadGuardKey{}, true)
	for _, test := range []struct {
		name string
		run  func() error
	}{
		{"query", func() error {
			var count int
			return f.service.database.db.QueryRowContext(ctx, "SELECT count(*) FROM projects").Scan(&count)
		}},
		{"read-only transaction", func() error {
			tx, err := f.service.database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err == nil {
				err = tx.Rollback()
			}
			return err
		}},
		{"nil transaction options", func() error {
			tx, err := f.service.database.db.BeginTx(ctx, nil)
			if err == nil {
				err = tx.Rollback()
			}
			return err
		}},
		{"default transaction options", func() error {
			tx, err := f.service.database.db.BeginTx(ctx, &sql.TxOptions{})
			if err == nil {
				err = tx.Rollback()
			}
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil || !strings.Contains(err.Error(), "read-only GET used the writer") {
				t.Fatalf("guard error=%v", err)
			}
		})
	}
	if len(guard.violations) != 4 {
		t.Fatalf("guard recorded %d violations", len(guard.violations))
	}
}

func hubWriterReadOperation(call *ast.CallExpr, writerAliases map[string]bool) string {
	name := ""
	writer := false
	switch method := call.Fun.(type) {
	case *ast.SelectorExpr:
		name = method.Sel.Name
		if name == "Query" || name == "QueryRow" || name == "QueryContext" || name == "QueryRowContext" || name == "Begin" || name == "BeginTx" {
			if receiver, ok := method.X.(*ast.SelectorExpr); ok && receiver.Sel.Name == "db" {
				writer = true
			}
			if receiver, ok := method.X.(*ast.Ident); ok && writerAliases[receiver.Name] {
				writer = true
			}
		}
	case *ast.Ident:
		name = method.Name
	}
	if strings.HasPrefix(name, "read") || strings.HasPrefix(name, "list") || strings.HasPrefix(name, "load") {
		for _, argument := range call.Args {
			if receiver, ok := argument.(*ast.SelectorExpr); ok && receiver.Sel.Name == "db" {
				writer = true
			}
		}
	}
	if writer {
		return name
	}
	return ""
}

func TestHubWriterReadOperations(t *testing.T) {
	for _, test := range []struct {
		expression string
		operation  string
	}{
		{"s.database.db.QueryContext(ctx, query)", "QueryContext"},
		{"s.database.db.QueryRowContext(ctx, query)", "QueryRowContext"},
		{"s.database.db.Query(query)", "Query"},
		{"s.database.db.QueryRow(query)", "QueryRow"},
		{"s.database.db.BeginTx(ctx, nil)", "BeginTx"},
		{"s.database.db.BeginTx(ctx, &sql.TxOptions{})", "BeginTx"},
		{"s.database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: false})", "BeginTx"},
		{"s.database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})", "BeginTx"},
		{"s.database.db.BeginTx(ctx, opts)", "BeginTx"},
		{"s.database.db.Begin()", "Begin"},
		{"writer.BeginTx(ctx, nil)", "BeginTx"},
		{"reader.BeginTx(ctx, nil)", ""},
		{"readProject(ctx, s.database.db)", "readProject"},
		{"s.database.reader.BeginTx(ctx, nil)", ""},
		{"s.database.reader.QueryContext(ctx, query)", ""},
	} {
		t.Run(test.expression, func(t *testing.T) {
			expression, err := parser.ParseExpr(test.expression)
			if err != nil {
				t.Fatal(err)
			}
			if got := hubWriterReadOperation(expression.(*ast.CallExpr), map[string]bool{"writer": true}); got != test.operation {
				t.Fatalf("writer operation=%q, want %q", got, test.operation)
			}
		})
	}
}

func TestHubWriterQueriesRequireAnOwnedReadReason(t *testing.T) {
	reasons := map[string]string{
		"createHostedProjectJSON":      "Project readback follows creation in this request.",
		"advanceGitHubImport":          "Import readback follows the advance performed by this request.",
		"advanceGitHubBatch":           "The import loop reads back its own preceding batch update.",
		"setSlackIntegration":          "Integration readback follows its transactional update in this request.",
		"openDatabase":                 "Startup verifies the writer's schema and hosted binding before serving requests.",
		"configure":                    "Startup verifies settings on the actual writer connection.",
		"initializeAutoVacuum":         "Startup checks for an empty database before setting its vacuum mode on the writer, before opening readers.",
		"enableWAL":                    "Changing journal mode is a database write.",
		"verifyIdentity":               "Startup reads back the application identity it writes on the same connection.",
		"consumeHostedTransaction":     "UPDATE RETURNING atomically consumes a login transaction.",
		"unsubscribeIssueEmail":        "UPDATE RETURNING atomically disables the issue subscription for the unsubscribe link.",
		"attemptDiffBodyStored":        "The readback verifies the diff-body update just performed by this request.",
		"vacuumAttemptDiffBodies":      "Offline maintenance measures the writer connection before vacuuming.",
		"ensureHostedCustomer":         "The billing-intent readback follows its idempotent writer insertion.",
		"claimHostedOperation":         "Idempotent command readback observes the writer insertion in this request.",
		"tenantRequestMetrics.report":  "Request metrics use their independent database, outside the tenant writer pool.",
		"tenantRequestMetrics.history": "Request metrics use their independent database, outside the tenant writer pool.",
		"bindHostedDatabase":           "Startup verifies binding through the connection that installs it.",
		"createNativeOrganizationFor":  "The slug readback follows the organization insert in this request.",
		"nativeIssueJSON":              "The mutation response reads back the browser path of the issue this request just wrote.",
	}
	transactionReasons := map[string]string{
		"nativeProgressOwner.Publish":       "Atomically publishes one fenced progress decision, its bounded evidence and comment, and one deduplicated machine intake occurrence.",
		"requestTeamUnread":                 "Writes only the caller's unread refresh intent and supplied project scope; history projection runs in maintenance and summary reads use the reader.",
		"advanceTeamUnread":                 "Publishes one reader-planned, bounded unread summary page and its cursor under the existing board maintenance owner.",
		"teamThreadOperation":               "Updates one thread's status after rechecking the caller's mutation authority.",
		"createTeamThread":                  "Creates one thread and its request-supplied references after rechecking mutation authority.",
		"postTeamThread":                    "Appends one post and its request-supplied references, and updates only its thread and author's participant row atomically.",
		"writeConversationQueryMetadata":    "Backfills one source row per transaction, conditional on its unchanged payload, under the existing retention owner.",
		"recordSpriteWakeFacts":             "The Sprite lifecycle owner atomically updates the candidate machine's wake result and matching typed facts.",
		"populateRunnerFact":                "Retention verifies one unchanged source row and writes that row's typed facts per transaction.",
		"populateHostedImportField":         "Projects one reader-evaluated legacy source row with a payload comparison in the existing retention owner.",
		"convertIssuePageFact":              "Writes one source-checked issue, event, board or version projection after decoding on the reader pool in the existing storage maintenance owner.",
		"convertActivityAttempt":            "Backfills one attempt and its owned phase and duration projections after verifying its current source.",
		"applyHealthProjection":             "Converts one source row after a conditional write fences its source and projection state.",
		"writeTimeIndexRecord":              "Writes one historical timestamp and its durable cursor per retention transaction.",
		"populateChangeColumn":              "Updates one owned typed projection after decoding and comparing its source through the reader pool.",
		"populateClaimFact":                 "Populates at most 100 unknown claim projections from their source rows in the existing retention transaction owner.",
		"convertAttemptLeaseOnce":           "Rechecks and terminalizes one superseded attempt and its lease on the existing storage conversion tick.",
		"convertAttemptHotProjection":       "Converts one attempt projection after checking its source row under the write transaction.",
		"convertNativeBoardRank":            "Converts one project workflow rank set after checking its source under the write transaction.",
		"convertRunnerBoardRow":             "Converts one runner project diagnostic snapshot after checking its source under the write transaction.",
		"prepareContents":                   "The request reserves and verifies its own archive catalog rows and storage allowance before uploading content.",
		"accountProjectIcon":                "The request reserves its own icon attachment row and storage allowance, rechecking authority, before uploading content.",
		"collectIssueEmails":                "Queues notification batches and advances their event cursors atomically.",
		"deliverDailySummary":               "Claims one recipient window and records its delivery outcome in separate short transactions outside the send.",
		"claimIssueEmail":                   "Deletes a due notification batch and reads its recipient in the same transaction before delivery.",
		"issueNotificationPreference":       "Updates the member preference and deletes queued notifications atomically on PUT; GET uses the reader.",
		"revokeHostedInvitationFor":         "Revokes the invitation and releases its reserved seat atomically.",
		"sendReservedHostedInvitationFor":   "Records the invitation together with its grants and reserved seat.",
		"editHostedInvitationFor":           "Updates invitation grants under current authority.",
		"creditEvent":                       "Applies a billing credit event once and records its receipt.",
		"autoFund":                          "Reserves funding before dispatching the billing operation.",
		"changeDependency":                  "Applies a dependency mutation and its event atomically.",
		"changeQueueOrder":                  "Applies a queue order mutation and its event atomically.",
		"writeAttachmentArchive":            "Atomically charges an immutable archive reservation or publishes its verified work-item reference, with a readback of the owned row.",
		"artifactReceipt":                   "Records an artifact receipt under its fenced upload grant.",
		"authorizeArtifactUpload":           "Creates a fenced artifact upload grant.",
		"artifactReadGrantCommand":          "Creates an artifact read grant as an explicit command.",
		"commitHubTransaction":              "Runs the supplied Hub mutation and commits its resulting state.",
		"checkAttachmentEvidence":           "Records attachment evidence inside the attempt authority transaction.",
		"RecordConversationUsage":           "Writes conversation usage and its billing consumption.",
		"deleteCloudAttachmentCommand":      "Deletes the attachment and records its native event.",
		"referenceCloudAttachmentCommand":   "Adds an attachment source reference and records its event.",
		"transact":                          "Runs the supplied conversation mutation and commits its resulting state.",
		"conversationTurnCoordinator.write": "Runs the turn mutation after rechecking conversation authority.",
		"normalizeAfterRestart":             "Normalizes interrupted conversation state during startup.",
		"convertConversationPayload":        "Converts one reader-selected message or event payload and its metadata after rechecking the conversion stamp.",
		"convertConversationToolPayload":    "Converts one reader-selected tool revision and records its archive pointer and conversion progress after rechecking the conversion stamp.",
		"convertConversationRecordPayload":  "Converts one reader-selected conversation record payload and its metadata after rechecking the conversion stamp.",
		"materializeRestartedMessages":      "Updates one interrupted message and its prepared archive pointer after rechecking its revision and delivery state.",
		"normalizeRestartedExecutions":      "Updates one interrupted conversation execution and its prepared archive pointer using the observed revision and payload as compare-and-swap predicates.",
		"normalizeRestartedReceipts":        "Updates one interrupted command receipt and its prepared archive pointer using the observed payload as a compare-and-swap predicate.",
		"createAPITokenFor":                 "Creates a token together with its grants.",
		"grantNativeTokenFor":               "Changes a native token grant under current authority.",
		"recordWebhook":                     "Inserts the webhook inbox receipt once.",
		"processWebhook":                    "Applies a webhook payload and advances the inbox receipt.",
		"commitHealthEvaluation":            "Persists computed health findings and their history after legacy cancellation evaluation on the reader pool.",
		"revokeHostedMemberLocally":         "Revokes local membership, tokens, sessions and grants.",
		"hostedGrant":                       "Changes a member project grant under current authority.",
		"createHostedProjectRecord":         "Creates the project together with its workflow and initial grants.",
		"hostedArtifactAllowances":          "Records artifact usage before enforcing hosted allowances.",
		"storeHostedMember":                 "Upserts the authenticated member and its principal grants.",
		"saveHostedCheckout":                "Persists the checkout intent and its billing action.",
		"recordBillingAction":               "Records a billing action and its consumption.",
		"configureHostedBilling":            "Installs hosted billing settings at startup.",
		"commitHostedBilling":               "Commits the reconciled billing state and consumed events.",
		"acceptHostedInvitationReference":   "Consumes an invitation and installs the accepted membership.",
		"applyHostedPlanCommand":            "Changes the plan and records its command receipt.",
		"configureHostedPlans":              "Installs hosted plan configuration at startup.",
		"applyHostedRuntime":                "Applies the signed hosted runtime update and revocations.",
		"sharedHostedSession":               "Creates a missing shared-entry session and its support audit.",
		"revokeHostedSharedBinding":         "Revokes the shared session and its conversation authority.",
		"MigrateHostedSharedOrigin":         "Offline migration writes the shared-entry origin bindings.",
		"recordHostedRequest":               "Hosted GET accounting updates consumption in its own write transaction.",
		"reserveHostedInvitationSeat":       "Reserves an invitation seat while checking the plan allowance.",
		"commitNativeMutation":              "Runs a native mutation with authority, event and replay receipt in one transaction.",
		"populateArchiveFact":               "Writes one unknown archive projection after reader-pool decoding, fenced by its unchanged source payload.",
		"populateTimeIndex":                 "Populates one bounded chunk of typed timestamp facts and its cursor on the existing retention tick.",
		"backfillNativeSearch":              "Indexes one source document selected on the reader and advances its rebuild or dirty cursor in the same transaction.",
		"expireNativeRecords":               "Deletes one bounded batch of expired native records on the existing maintenance tick.",
		"publishStorageArchive":             "Commits one issue archive pointer and its bounded section metadata after rechecking the exact issue and conversation revisions.",
		"retireStorageArchiveSections":      "Rechecks the archived issue before atomically retiring at most 500 copied rows and advancing their durable section cursor.",
		"discoverStorageRow":                "Queues one source row and advances its typed discovery state atomically on the existing conversion tick.",
		"convertObjectPayloadFamily":        "Replaces one verified payload with its typed reference and conversion progress after comparing the owned source row.",
		"convertStorageAggregate":           "Commits one verified immutable reference and retires its owned inline row with the progress cursor after rechecking the aggregate and source snapshot.",
		"compactRetainedEvent":              "Deletes one bounded batch of obsolete event history for one aggregate planned on the reader, on the existing maintenance tick.",
		"compactAnalyticsSamplesWithClock":  "Deletes one bounded batch of skip samples already counted by hourly rollups, on the existing maintenance tick.",
		"reclaimStorage":                    "Reclaims at most 128 free pages in one short transaction on the existing retention tick.",
		"backfillChangeSource":              "Converts one legacy source representation after reader-pool validation on the existing maintenance tick.",
		"backfillAnalyticsRollups":          "Projects one bounded batch of a project's work items and their positions on the existing maintenance tick.",
		"drainAnalyticsItem":                "Applies one bounded reader-planned analytics page after checking source revision and progress, or removes the item's orphaned position and dirty marker.",
		"drainNativeBoardDirtyAfter":        "Projects one dirty board card and consumes its marker in one background write transaction.",
		"trimNextDashboardProject":          "Trims at most 128 feed rows for one over-limit project on the existing board maintenance tick.",
		"executeAttachmentDeletion":         "Records an attachment deletion mutation and replay receipt.",
		"operatorChatStore.Save":            "Persists and prunes operator chat sessions in their independent store.",
		"commitOutbox":                      "Applies the outbox mutation and records its delivery atomically.",
		"claimOutbox":                       "Claims an outbox delivery and increments its attempt.",
		"adjustAICredits":                   "Writes the credit adjustment and its audit record.",
		"approvePolicy":                     "Stores the approved policy and its authority record.",
		"revokePolicy":                      "Revokes the selected policy and its authority record.",
		"projectConfigurationCommand":       "Writes runner configuration commands; inspection and preview use the reader.",
		"rewrapSecret":                      "Rewraps one provider secret and records its audit using a compare-and-swap update.",
		"secretMutation":                    "Applies the supplied secret mutation under current authority.",
		"createPoolSpriteNamed":             "Reserves and records a new pool sprite and its enrollment.",
		"deletePoolSprite":                  "Records removal of the pool sprite and its enrollment.",
		"bootstrapPoolSprite":               "Enrolls the bootstrapped member only while current intent still wants it.",
		"cancelPoolSpriteReservation":       "Cancels a stale reservation and its enrollment before paid creation.",
		"retirePoolSpriteReservation":       "Retires a failed or stale reservation and counts paid provisioning failures.",
		"applyReconcileSnapshot":            "Persists the reconcile snapshot, work items and cursor.",
		"prepareRestore":                    "Offline restore replaces the administrator credential and writes recovery state.",
		"runnerAdminTransaction":            "Applies a runner administration mutation with allowance checks.",
		"updateRunnerCapacityCommand":       "Writes capacity configuration with its revision and command receipt.",
		"runnerTransaction":                 "Applies a runner protocol mutation under current runner authority.",
		"chargeRunnerRead":                  "Charges runner read consumption as an explicit accounting write.",
		"applyRunnerUpdateCommand":          "Writes the selected runner update and its command receipt.",
		"processSlackDelivery":              "Claims and records a Slack delivery attempt.",
		"Claim":                             "Claims the work item and reserves its fenced lease.",
		"renew":                             "Renews the fenced work lease.",
		"releaseLeaseOnce":                  "Releases the fenced work lease and, when requested, cancels its owned attempt atomically.",
		"appendEvent":                       "Writes the worker event and resulting work state.",
		"applyObservedDefaultBranchPolicy":  "Persists the observed default-branch policy and approval.",
		"tenantRequestMetrics.write":        "Persists request metrics in their independent database.",
		"advanceGitHubBatch":                "The import loop applies its next batch and reads back the update.",
		"ensureHostedCustomer":              "Billing customer creation reserves its intent and reads back that insertion.",
		"bindHostedDatabase":                "Startup installs and verifies the hosted binding on the writer.",
		"syncOrganizationSlug":              "Writes the organization slug received from the shared entry.",
		"savedWorkViews":                    "Writes one owned saved view, its selections and the current user's favorite; GET uses the reader.",
		"slugSettings":                      "Updates the organization or project slug under current authority; GET uses the reader.",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, contents, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			writerAliases := make(map[string]bool)
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if assignment, ok := node.(*ast.AssignStmt); ok {
					for index, value := range assignment.Rhs {
						if receiver, ok := value.(*ast.SelectorExpr); ok && receiver.Sel.Name == "db" && index < len(assignment.Lhs) {
							if alias, ok := assignment.Lhs[index].(*ast.Ident); ok {
								writerAliases[alias.Name] = true
							}
						}
					}
				}
				return true
			})
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := hubWriterReadOperation(call, writerAliases)
				if name == "" {
					return true
				}
				ownedReasons := reasons
				if name == "Begin" || name == "BeginTx" {
					ownedReasons = transactionReasons
				}
				reason := ownedReasons[fn.Name.Name]
				if fn.Recv != nil {
					receiver := fn.Recv.List[0].Type
					if pointer, ok := receiver.(*ast.StarExpr); ok {
						receiver = pointer.X
					}
					if receiverType, ok := receiver.(*ast.Ident); ok {
						if scoped := ownedReasons[receiverType.Name+"."+fn.Name.Name]; scoped != "" {
							reason = scoped
						}
					}
				}
				if reason == "" {
					t.Errorf("%s: %s uses writer %s without an owned read or write reason", path, fn.Name.Name, name)
				}
				return true
			})
		}
	}
}

func TestReadPoolExhaustionReturnsLoggedDeadline(t *testing.T) {
	var logs hostedLogBuffer
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	f := newNativeFixture(t, service, "", "exhausted-readers")
	connections := make([]*sql.Conn, 0, readerConnections)
	for range readerConnections {
		conn, err := service.database.reader.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
	}
	release := func() {
		for _, conn := range connections {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		}
		connections = nil
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, f.base+"/onboarding", nil)
	request.Header.Set("Authorization", "Bearer "+f.token)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("pool exhaustion response=%d context=%v", response.Code, ctx.Err())
	}
	if !strings.Contains(logs.String(), "context deadline exceeded") || !strings.Contains(logs.String(), "read_timeout_cause=reader_pool_exhausted") {
		t.Fatalf("pool exhaustion lost its cause: %s", logs.String())
	}
	if service.database.reader.Stats().WaitCount == 0 {
		t.Fatal("request did not wait for the exhausted reader pool")
	}
	release()
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, f.base+"/onboarding", f.token, nil), http.StatusOK)
}

func installReadQueryCounter(t *testing.T, d *database) *writerReadGuard {
	t.Helper()
	counter := &writerReadGuard{}
	original := d.reader
	pool := sql.OpenDB(guardedWriterConnector{timedConnector: timedConnector{dsn: sqliteReaderDSN(d.path, defaultBusyTimeout), driver: original.Driver()}, guard: counter})
	pool.SetMaxOpenConns(readerConnections)
	d.reader = pool
	t.Cleanup(func() {
		d.reader = original
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	return counter
}

type auditDeadlineConnector struct {
	timedConnector
	inserts, bounded *atomic.Int64
}

func (c auditDeadlineConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.timedConnector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &auditDeadlineConn{timedConn: conn.(*timedConn), connector: c}, nil
}

type auditDeadlineConn struct {
	*timedConn
	connector auditDeadlineConnector
}

func (c *auditDeadlineConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(query, "secret_audit") {
		c.connector.inserts.Add(1)
		if _, ok := ctx.Deadline(); ok {
			c.connector.bounded.Add(1)
		}
	}
	return c.timedConn.ExecContext(ctx, query, args)
}

func TestSecretUseAuditIsNotBoundedByReadDeadline(t *testing.T) {
	f := newNativeFixture(t, nil, "", "audit-deadline")
	now := time.Now().UTC()
	configureTestSlack(t, f, &now, true)
	var inserts, bounded atomic.Int64
	original := f.service.database.db
	pool := sql.OpenDB(auditDeadlineConnector{timedConnector: timedConnector{dsn: sqliteWriterDSN(f.service.database.path, f.service.config.BusyTimeout, false), driver: original.Driver()}, inserts: &inserts, bounded: &bounded})
	pool.SetMaxOpenConns(1)
	f.service.database.db = pool
	t.Cleanup(func() {
		f.service.database.db = original
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	value, err := f.service.readSlackWebhook(t.Context(), f.project.OrganizationID, "test")
	if err != nil || len(value) == 0 {
		t.Fatalf("webhook read: %v", err)
	}
	clear(value)
	if inserts.Load() != 1 || bounded.Load() != 0 {
		t.Fatalf("secret use audit inserts=%d bounded by read deadline=%d", inserts.Load(), bounded.Load())
	}
}
