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
			"/issue-notification-preference",
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
		"prepareContents":                   "The request reserves and verifies its own archive catalog rows and storage allowance before uploading content.",
		"collectIssueEmails":                "Queues notification batches and advances their event cursors atomically.",
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
		"hubTransact":                       "Runs the supplied Hub mutation and commits its resulting state.",
		"checkAttachmentEvidence":           "Records attachment evidence inside the attempt authority transaction.",
		"RecordConversationUsage":           "Writes conversation usage and its billing consumption.",
		"deleteCloudAttachmentCommand":      "Deletes the attachment and records its native event.",
		"referenceCloudAttachmentCommand":   "Adds an attachment source reference and records its event.",
		"transact":                          "Runs the supplied conversation mutation and commits its resulting state.",
		"conversationTurnCoordinator.write": "Runs the turn mutation after rechecking conversation authority.",
		"normalizeAfterRestart":             "Normalizes interrupted conversation state during startup.",
		"createAPITokenFor":                 "Creates a token together with its grants.",
		"grantNativeTokenFor":               "Changes a native token grant under current authority.",
		"recordWebhook":                     "Inserts the webhook inbox receipt once.",
		"processWebhook":                    "Applies a webhook payload and advances the inbox receipt.",
		"evaluateOrganizationHealth":        "Persists computed health findings and their history.",
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
		"executeNativeMutation":             "Runs a native mutation with authority, event and replay receipt in one transaction.",
		"populateTimeIndex":                 "Populates one bounded chunk of typed timestamp facts and its cursor on the existing retention tick.",
		"expireNativeRecords":               "Deletes one bounded batch of expired native records on the existing maintenance tick.",
		"compactRetainedEvents":             "Deletes one bounded batch of obsolete event history planned on the reader, on the existing maintenance tick.",
		"compactAnalyticsSamples":           "Deletes one bounded batch of skip samples already counted by hourly rollups, on the existing maintenance tick.",
		"backfillChangeSource":              "Converts one legacy source representation after reader-pool validation on the existing maintenance tick.",
		"backfillAnalyticsRollups":          "Projects one bounded batch of a project's work items and their positions on the existing maintenance tick.",
		"drainAnalyticsDirty":               "Projects one bounded batch of deferred analytics work items on the existing maintenance tick.",
		"drainNativeBoardDirty":             "Projects one dirty board card and consumes its marker in one background write transaction.",
		"retryFailedAnalyticsRollups":       "Reprojects one failed work item and clears its failure mark on the existing maintenance tick.",
		"executeAttachmentDeletion":         "Records an attachment deletion mutation and replay receipt.",
		"operatorChatStore.Save":            "Persists and prunes operator chat sessions in their independent store.",
		"commitOutbox":                      "Applies the outbox mutation and records its delivery atomically.",
		"claimOutbox":                       "Claims an outbox delivery and increments its attempt.",
		"adjustAICredits":                   "Writes the credit adjustment and its audit record.",
		"approvePolicy":                     "Stores the approved policy and its authority record.",
		"revokePolicy":                      "Revokes the selected policy and its authority record.",
		"projectConfigurationCommand":       "Writes runner configuration commands; inspection and preview use the reader.",
		"secretMutation":                    "Applies the supplied secret mutation under current authority.",
		"RotateProjectSecrets":              "Offline rotation rewrites encrypted project secrets.",
		"createPoolSprite":                  "Reserves and records a new pool sprite and its enrollment.",
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
		"releaseLease":                      "Releases the fenced work lease and, when requested, cancels its owned attempt atomically.",
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
