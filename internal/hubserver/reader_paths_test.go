package hubserver

import (
	"bytes"
	"context"
	"database/sql"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
)

type writerReadGuardKey struct{}

type writerReadGuard struct {
	mu           sync.Mutex
	violations   []string
	queries      atomic.Int64
	transactions atomic.Int64
	readOnly     map[*pgx.Conn]bool
	repository   *postgres.Repository
}

var writerReadAllowlist = map[string]string{
	"recordHostedRequest": "Hosted GET request accounting updates consumption in its own write transaction.",
	"recordAccessAudit":   "Support-session GETs record their access audit in its own write transaction.",
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

func (g *writerReadGuard) traceStatement(ctx context.Context, conn *pgx.Conn, query string) context.Context {
	statement := strings.ToUpper(strings.TrimSpace(query))
	if strings.HasPrefix(statement, "BEGIN") || strings.HasPrefix(statement, "START TRANSACTION") {
		readOnly := strings.Contains(statement, " READ ONLY")
		g.mu.Lock()
		if g.readOnly == nil {
			g.readOnly = make(map[*pgx.Conn]bool)
		}
		g.readOnly[conn] = readOnly
		g.mu.Unlock()
		if marked, _ := ctx.Value(writerReadGuardKey{}).(bool); marked && readOnly {
			g.transactions.Add(1)
		}
		if !readOnly {
			if err := g.check(ctx, "PostgreSQL read-write transaction"); err != nil {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				return canceled
			}
		}
		return ctx
	}
	if statement == "COMMIT" || statement == "ROLLBACK" {
		g.mu.Lock()
		delete(g.readOnly, conn)
		g.mu.Unlock()
		return ctx
	}
	if marked, _ := ctx.Value(writerReadGuardKey{}).(bool); marked {
		g.queries.Add(1)
		g.mu.Lock()
		readOnly := g.readOnly[conn]
		g.mu.Unlock()
		if !readOnly {
			if err := g.check(ctx, "PostgreSQL query outside a read-only transaction"); err != nil {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				return canceled
			}
		}
	}
	return ctx
}

func (g *writerReadGuard) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return g.traceStatement(ctx, conn, data.SQL)
}

func (g *writerReadGuard) TraceQueryEnd(_ context.Context, conn *pgx.Conn, _ pgx.TraceQueryEndData) {
	if conn.PgConn().TxStatus() == 'I' {
		g.mu.Lock()
		delete(g.readOnly, conn)
		g.mu.Unlock()
	}
}

func (g *writerReadGuard) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		ctx = g.traceStatement(ctx, conn, query.SQL)
		if ctx.Err() != nil {
			return ctx
		}
	}
	return ctx
}

func (*writerReadGuard) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (g *writerReadGuard) TraceBatchEnd(_ context.Context, conn *pgx.Conn, _ pgx.TraceBatchEndData) {
	if conn.PgConn().TxStatus() == 'I' {
		g.mu.Lock()
		delete(g.readOnly, conn)
		g.mu.Unlock()
	}
}

func installWriterReadGuard(t *testing.T, s *Service, guard *writerReadGuard) *writerReadGuard {
	t.Helper()
	if guard == nil || guard.repository == nil || s.database.postgres != guard.repository || s.config.TenantRepository != guard.repository || s.config.AccessRepository != guard.repository || s.database.billingRepository != guard.repository || s.config.BillingRepository != guard.repository || s.requestMetrics == nil || s.requestMetrics.Repository != guard.repository {
		t.Fatal("read guard does not cover every actual PostgreSQL owner")
	}
	ctx := context.WithValue(t.Context(), writerReadGuardKey{}, true)
	called := false
	err := s.database.postgres.WriteTenant(ctx, s.database.hostedOrganization, corestore.Serving, func(tenantstore.Writer) error {
		called = true
		return nil
	})
	guard.mu.Lock()
	violations := len(guard.violations)
	guard.violations = nil
	guard.mu.Unlock()
	if err == nil || called || violations == 0 {
		t.Fatalf("guard missed actual PostgreSQL writer: callback=%v violations=%d error=%v", called, violations, err)
	}
	if err := s.database.postgres.ReadTenant(ctx, s.database.hostedOrganization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.AccessOrganization(ctx)
		return err
	}); err != nil {
		t.Fatalf("guard refused actual PostgreSQL reader: %v", err)
	}
	if guard.queries.Load() == 0 || guard.transactions.Load() == 0 {
		t.Fatal("guard did not observe actual PostgreSQL read-only transaction and query")
	}
	guard.queries.Store(0)
	guard.transactions.Store(0)
	s.echo.Pre(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().Method == http.MethodGet {
				c.SetRequest(c.Request().WithContext(context.WithValue(c.Request().Context(), writerReadGuardKey{}, true)))
			}
			return next(c)
		}
	})
	return guard
}

func newWriterGuardNativeFixture(t *testing.T) (nativeFixture, *writerReadGuard) {
	t.Helper()
	guard := &writerReadGuard{}
	repository, _ := projectRecordPostgresWithAdmin(t, 4, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, guard)
	})
	guard.repository = repository
	service := openLocalPostgresHubTestService(t, repository, Config{SecretKeys: secretTestKeys(t, "1", "1")})
	organization := nativeOrganization{ID: "org_first", Name: "Example organization", Slug: "example-first"}
	seedPostgresHubAdmin(t, repository, service, organization)
	key := bytes.Repeat([]byte{0x5a}, 32)
	envelope, err := service.config.SecretKeys.Seal(key, accessstore.SecretAAD(organization.ID, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	now := service.config.Clock().UTC().Truncate(time.Microsecond)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	service.localOrganization = string(organization.ID)
	service.database.hostedOrganization = organization.ID
	service.requestMetrics.Organization = organization.ID
	return newNativeFixture(t, service, organization.ID, "reader-guard"), guard
}

func newWriterGuardHostedFixture(t *testing.T, listen bool, configure ...func(*Config)) (*browserHostedFixture, *writerReadGuard) {
	t.Helper()
	guard := &writerReadGuard{}
	open := func(t *testing.T, cfg Config) *Service {
		t.Helper()
		cfg = hostedPostgresConfig(t, cfg)
		value, ok := projectRecordDatabases.Load(cfg.Postgres.Connection.Database)
		if !ok {
			t.Fatal("guarded hosted PostgreSQL fixture database is unavailable")
		}
		admin := value.(*sql.DB)
		config := postgresStartupTemplate.NativeConfig(t, admin, cfg.Postgres.Connection.User, cfg.Postgres.Connection.Password)
		config.MaxConns = int32(cfg.Postgres.Pool.MaxConnections)
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, guard)
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		repository, err := postgres.NewRepository(t.Context(), pool, cfg.Postgres.Pool)
		if err != nil {
			pool.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := repository.Close(); err != nil {
				t.Error(err)
			}
		})
		projectRecordDatabases.Store(repository, admin)
		t.Cleanup(func() { projectRecordDatabases.Delete(repository) })
		guard.repository = repository
		return openPostgresHubTestService(t, repository, cfg)
	}
	fixture := newBrowserHostedFixtureBackendWithService(t, true, "org_browser_preview", listen, open, configure...)
	return fixture, guard
}

func TestReadOnlyGETsNeverAcquireWriter(t *testing.T) {
	t.Parallel()
	t.Run("native", func(t *testing.T) {
		f, guard := newWriterGuardNativeFixture(t)
		issue := f.create(t, "readable issue")
		approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
		runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
		runner.enroll(t)
		installWriterReadGuard(t, f.service, guard)
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
		if guard.queries.Load() == 0 || guard.transactions.Load() == 0 {
			t.Fatal("GET matrix did not use traced PostgreSQL read-only transactions")
		}
	})
	t.Run("hosted", func(t *testing.T) {
		f, guard := newWriterGuardHostedFixture(t, false, func(cfg *Config) {
			cfg.Hosted.EntitlementAdminToken = []byte(strings.Repeat("e", 32))
			cfg.Hosted.EntitlementAdministrator = "test-operator"
		})
		installWriterReadGuard(t, f.service, guard)
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
		if guard.queries.Load() == 0 || guard.transactions.Load() == 0 {
			t.Fatal("GET matrix did not use traced PostgreSQL read-only transactions")
		}
	})
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
	t.Parallel()
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
	t.Parallel()
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
		"nativeProgressOwner.Publish":         "Atomically publishes one fenced progress decision, its bounded evidence and comment, and one deduplicated machine intake occurrence.",
		"requestTeamUnread":                   "Writes only the caller's unread refresh intent and supplied project scope; history projection runs in maintenance and summary reads use the reader.",
		"advanceTeamUnread":                   "Publishes one reader-planned, bounded unread summary page and its cursor under the existing board maintenance owner.",
		"teamThreadOperation":                 "Updates one thread's status after rechecking the caller's mutation authority.",
		"createTeamThread":                    "Creates one thread and its request-supplied references after rechecking mutation authority.",
		"postTeamThread":                      "Appends one post and its request-supplied references, and updates only its thread and author's participant row atomically.",
		"recordSpriteWakeFacts":               "The Sprite lifecycle owner atomically updates the candidate machine's wake result and matching typed facts.",
		"populateRunnerFact":                  "Retention verifies one unchanged source row and writes that row's typed facts per transaction.",
		"convertActivityAttempt":              "Backfills one attempt and its owned phase and duration projections after verifying its current source.",
		"applyHealthProjection":               "Converts one source row after a conditional write fences its source and projection state.",
		"populateChangeColumn":                "Updates one owned typed projection after decoding and comparing its source through the reader pool.",
		"populateClaimFact":                   "Populates at most 100 unknown claim projections from their source rows in the existing retention transaction owner.",
		"convertAttemptLeaseOnce":             "Rechecks and terminalizes one superseded attempt and its lease on the existing storage conversion tick.",
		"convertAttemptHotProjection":         "Converts one attempt projection after checking its source row under the write transaction.",
		"convertNativeBoardRank":              "Converts one project workflow rank set after checking its source under the write transaction.",
		"convertRunnerBoardRow":               "Converts one runner project diagnostic snapshot after checking its source under the write transaction.",
		"prepareContents":                     "The request reserves and verifies its own archive catalog rows and storage allowance before uploading content.",
		"accountProjectIcon":                  "The request reserves its own icon attachment row and storage allowance, rechecking authority, before uploading content.",
		"collectIssueEmails":                  "Queues notification batches and advances their event cursors atomically.",
		"deliverDailySummary":                 "Claims one recipient window and records its delivery outcome in separate short transactions outside the send.",
		"claimIssueEmail":                     "Deletes a due notification batch and reads its recipient in the same transaction before delivery.",
		"issueNotificationPreference":         "Updates the member preference and deletes queued notifications atomically on PUT; GET uses the reader.",
		"revokeHostedInvitationFor":           "Revokes the invitation and releases its reserved seat atomically.",
		"sendReservedHostedInvitationFor":     "Records the invitation together with its grants and reserved seat.",
		"editHostedInvitationFor":             "Updates invitation grants under current authority.",
		"creditEvent":                         "Applies a billing credit event once and records its receipt.",
		"autoFund":                            "Reserves funding before dispatching the billing operation.",
		"changeDependency":                    "Applies a dependency mutation and its event atomically.",
		"changeQueueOrder":                    "Applies a queue order mutation and its event atomically.",
		"writeAttachmentArchive":              "Atomically charges an immutable archive reservation or publishes its verified work-item reference, with a readback of the owned row.",
		"artifactReceipt":                     "Records an artifact receipt under its fenced upload grant.",
		"authorizeArtifactUpload":             "Creates a fenced artifact upload grant.",
		"artifactReadGrantCommand":            "Creates an artifact read grant as an explicit command.",
		"commitHubTransaction":                "Runs the supplied Hub mutation and commits its resulting state.",
		"checkAttachmentEvidence":             "Records attachment evidence inside the attempt authority transaction.",
		"RecordConversationUsage":             "Writes conversation usage and its billing consumption.",
		"deleteCloudAttachmentCommand":        "Deletes the attachment and records its native event.",
		"referenceCloudAttachmentCommand":     "Adds an attachment source reference and records its event.",
		"transact":                            "Runs the supplied conversation mutation and commits its resulting state.",
		"conversationTurnCoordinator.write":   "Runs the turn mutation after rechecking conversation authority.",
		"normalizeAfterRestart":               "Normalizes interrupted conversation state during startup.",
		"convertConversationPayload":          "Converts one reader-selected message or event payload and its metadata after rechecking the conversion stamp.",
		"convertConversationToolPayload":      "Converts one reader-selected tool revision and records its archive pointer and conversion progress after rechecking the conversion stamp.",
		"convertConversationRecordPayload":    "Converts one reader-selected conversation record payload and its metadata after rechecking the conversion stamp.",
		"materializeRestartedMessages":        "Updates one interrupted message and its prepared archive pointer after rechecking its revision and delivery state.",
		"normalizeRestartedExecutions":        "Updates one interrupted conversation execution and its prepared archive pointer using the observed revision and payload as compare-and-swap predicates.",
		"normalizeRestartedReceipts":          "Updates one interrupted command receipt and its prepared archive pointer using the observed payload as a compare-and-swap predicate.",
		"createAPITokenFor":                   "Creates a token together with its grants.",
		"grantNativeTokenFor":                 "Changes a native token grant under current authority.",
		"recordWebhook":                       "Inserts the webhook inbox receipt once.",
		"processWebhook":                      "Applies a webhook payload and advances the inbox receipt.",
		"commitHealthEvaluation":              "Persists computed health findings and their history after legacy cancellation evaluation on the reader pool.",
		"revokeHostedMemberLocally":           "Revokes local membership, tokens, sessions and grants.",
		"hostedGrant":                         "Changes a member project grant under current authority.",
		"createHostedProjectRecord":           "Creates the project together with its workflow and initial grants.",
		"hostedArtifactAllowances":            "Records artifact usage before enforcing hosted allowances.",
		"storeHostedMember":                   "Upserts the authenticated member and its principal grants.",
		"saveHostedCheckout":                  "Persists the checkout intent and its billing action.",
		"recordBillingAction":                 "Records a billing action and its consumption.",
		"configureHostedBilling":              "Installs hosted billing settings at startup.",
		"commitHostedBilling":                 "Commits the reconciled billing state and consumed events.",
		"acceptHostedInvitationReference":     "Consumes an invitation and installs the accepted membership.",
		"applyHostedPlanCommand":              "Changes the plan and records its command receipt.",
		"executeWorkItemRecordMutation":       "Prepares immutable work-item content before one tenant transaction rechecks access, project revision and billing feature, then commits the record, event, version and command receipt.",
		"publishChangeVersionRecord":          "Prepares immutable version and source content before one tenant transaction rechecks authority and commits the version, Change revision, events and receipt.",
		"intakeLinkedIssueRecord":             "Archives one source snapshot before one tenant transaction rechecks access, lease and billing authority and commits source state, comments, events and receipt.",
		"configureHostedPlans":                "Installs hosted plan configuration at startup.",
		"applyHostedRuntime":                  "Applies the signed hosted runtime update and revocations.",
		"sharedHostedSession":                 "Creates a missing shared-entry session and its support audit.",
		"revokeHostedSharedBinding":           "Revokes the shared session and its conversation authority.",
		"MigrateHostedSharedOrigin":           "Offline migration writes the shared-entry origin bindings.",
		"writeHostedRequestWindow":            "The existing request metrics owner publishes one aggregated HTTP usage window per short background transaction.",
		"reserveHostedInvitationSeat":         "Reserves an invitation seat while checking the plan allowance.",
		"commitNativeMutation":                "Runs a native mutation with authority, event and replay receipt in one transaction.",
		"expireNativeRecords":                 "Deletes one bounded batch of expired native records on the existing maintenance tick.",
		"publishStorageArchive":               "Commits one issue archive pointer and its bounded section metadata after rechecking the exact issue and conversation revisions.",
		"retireStorageArchiveSections":        "Rechecks the archived issue before atomically retiring at most 500 copied rows and advancing their durable section cursor.",
		"discoverStorageRow":                  "Queues one source row and advances its typed discovery state atomically on the existing conversion tick.",
		"convertObjectPayloadRow":             "Replaces one verified payload with its typed reference and conversion progress after comparing the owned source row.",
		"convertStorageAggregate":             "Commits one verified immutable reference and retires its owned inline row with the progress cursor after rechecking the aggregate and source snapshot.",
		"compactAnalyticsSamplesWithClock":    "Deletes one bounded batch of skip samples already counted by hourly rollups, on the existing maintenance tick.",
		"backfillChangeSource":                "Converts one legacy source representation after reader-pool validation on the existing maintenance tick.",
		"populateChangeSession":               "Freezes one version session and its source attempt authority after incremental reader-pool history validation on the existing Change column tick.",
		"backfillAnalyticsRollups":            "Projects one bounded batch of a project's work items and their positions on the existing maintenance tick.",
		"drainAnalyticsItem":                  "Applies one bounded reader-planned analytics page after checking source revision and progress, or removes the item's orphaned position and dirty marker.",
		"trimNextDashboardProject":            "Trims at most 128 feed rows for one over-limit project on the existing board maintenance tick.",
		"executeAttachmentDeletion":           "Records an attachment deletion mutation and replay receipt.",
		"operatorChatStore.Save":              "Persists and prunes operator chat sessions in their independent store.",
		"commitOutbox":                        "Applies the outbox mutation and records its delivery atomically.",
		"claimOutbox":                         "Claims an outbox delivery and increments its attempt.",
		"adjustAICredits":                     "Writes the credit adjustment and its audit record.",
		"revokePolicy":                        "Revokes the selected policy and its authority record.",
		"commitProjectConfiguration":          "Writes runner configuration commands; inspection and preview use the reader.",
		"createPoolSpriteNamed":               "Reserves and records a new pool sprite and its enrollment.",
		"deletePoolSprite":                    "Records removal of the pool sprite and its enrollment.",
		"bootstrapPoolSprite":                 "Enrolls the bootstrapped member only while current intent still wants it.",
		"cancelPoolSpriteReservation":         "Cancels a stale reservation and its enrollment before paid creation.",
		"retirePoolSpriteReservation":         "Retires a failed or stale reservation and counts paid provisioning failures.",
		"applyReconcileSnapshot":              "Persists the reconcile snapshot, work items and cursor.",
		"prepareRestore":                      "Offline restore replaces the administrator credential and writes recovery state.",
		"commitRunnerAdminTransaction":        "Applies a runner administration mutation with allowance checks.",
		"persistRunnerCapacityCommand":        "Writes capacity configuration with its revision and command receipt.",
		"commitRunnerTransaction":             "Applies a runner protocol mutation under current runner authority.",
		"chargeRunnerRead":                    "Charges runner read consumption as an explicit accounting write.",
		"persistRunnerUpdateCommand":          "Writes the selected runner update and its command receipt.",
		"Claim":                               "Claims the work item and reserves its fenced lease.",
		"appendEvent":                         "Writes the worker event and resulting work state.",
		"tenantRequestMetrics.write":          "Persists request metrics in their independent database.",
		"Service.beginProjectRecordAuthority": "Opens one project record mutation transaction after rechecking the caller's project, hosted and runner authority.",
		"advanceGitHubBatch":                  "The import loop applies its next batch and reads back the update.",
		"ensureHostedCustomer":                "Billing customer creation reserves its intent and reads back that insertion.",
		"bindHostedDatabase":                  "Startup installs and verifies the hosted binding on the writer.",
		"syncOrganizationSlug":                "Writes the organization slug received from the shared entry.",
		"savedWorkViews":                      "Writes one owned saved view, its selections and the current user's favorite; GET uses the reader.",
		"slugSettings":                        "Updates the organization or project slug under current authority; GET uses the reader.",
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

type readPoolDeadlineKey struct{}

type readPoolDeadlineTrace struct {
	dispatchWaitPostgresTrace
}

func (p *readPoolDeadlineTrace) TraceAcquireStart(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireStartData) context.Context {
	ctx = p.dispatchWaitPostgresTrace.TraceAcquireStart(ctx, pool, data)
	if arm, ok := ctx.Value(readPoolDeadlineKey{}).(func(context.Context) context.Context); ok {
		return arm(ctx)
	}
	return ctx
}

func TestReadPoolExhaustionReturnsLoggedDeadline(t *testing.T) {
	t.Parallel()
	probe := &readPoolDeadlineTrace{}
	repository, _ := projectRecordPostgresWithAdmin(t, 3, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, probe)
	})
	var logs hostedLogBuffer
	service := openLocalPostgresHubTestService(t, repository, Config{
		Logger: slog.New(slog.NewTextHandler(&logs, nil)), SecretKeys: secretTestKeys(t, "1", "1"),
	})
	organization := nativeOrganization{ID: "org_first", Name: "Example organization", Slug: "example-first"}
	seedPostgresHubAdmin(t, repository, service, organization)
	service.localOrganization = string(organization.ID)
	service.database.hostedOrganization = organization.ID
	service.requestMetrics.Organization = organization.ID
	f := newNativeFixture(t, service, organization.ID, "exhausted-readers")
	pool := probe.pool.Load()
	if pool == nil || pool.Stat().MaxConns() != 3 {
		t.Fatal("fixture did not expose its three-connection PostgreSQL pool")
	}

	var held []*pgxpool.Conn
	release := func() {
		for _, conn := range held {
			conn.Release()
		}
		held = nil
	}
	t.Cleanup(release)
	var acquireCtx context.Context
	var canceledBefore int64
	saturate := true
	handler := echo.New()
	handler.GET(hubapi.NativeBase+"/onboarding", func(c echo.Context) error {
		if !saturate {
			return service.getOnboardingPage(c)
		}
		ctx := c.Request().Context()
		for range pool.Stat().MaxConns() {
			conn, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, conn)
		}
		stats := pool.Stat()
		if stats.AcquiredConns() != stats.MaxConns() {
			t.Fatalf("physical pool is not full: acquired=%d max=%d", stats.AcquiredConns(), stats.MaxConns())
		}
		canceledBefore = stats.CanceledAcquireCount()
		ctx = context.WithValue(ctx, readPoolDeadlineKey{}, func(ctx context.Context) context.Context {
			if acquireCtx == nil {
				var cancel context.CancelFunc
				acquireCtx, cancel = context.WithDeadline(ctx, time.Time{})
				t.Cleanup(cancel)
			}
			return acquireCtx
		})
		c.SetRequest(c.Request().WithContext(ctx))
		return service.getOnboardingPage(c)
	}, service.requireAPIScope(hubapi.APIScopeOperator, hubapi.APIScopeWorker, hubapi.APIScopeAdmin))

	request := newHubAPIRequest(t, http.MethodGet, f.base+"/onboarding", f.token, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request.WithContext(testenv.WaitContext(t, t.Context())))
	if acquireCtx == nil {
		t.Fatalf("request never reached physical acquire: status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Code != http.StatusInternalServerError || !errors.Is(acquireCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("pool exhaustion response=%d acquire context=%v", response.Code, acquireCtx.Err())
	}
	canceledAfter := pool.Stat().CanceledAcquireCount()
	if delta := canceledAfter - canceledBefore; delta != 1 {
		t.Fatalf("canceled PostgreSQL acquires = %d, want 1", delta)
	}
	var deadlines int
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, "read_timeout_cause=read_deadline") {
			continue
		}
		deadlines++
		for _, field := range []string{"context deadline exceeded", "pool_full=true", "pool_in_use=3", "pool_max=3", "pool_canceled_acquires=" + strconv.FormatInt(canceledAfter, 10)} {
			if !strings.Contains(line, field) {
				t.Errorf("pool exhaustion log omitted %q: %s", field, line)
			}
		}
	}
	if deadlines != 1 {
		t.Fatalf("logged deadline records = %d, want 1: %s", deadlines, logs.String())
	}
	release()
	saturate = false
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, newHubAPIRequest(t, http.MethodGet, f.base+"/onboarding", f.token, nil).WithContext(t.Context()))
	requireNativeStatus(t, response, http.StatusOK)
}

func installReadQueryCounter(config *pgxpool.Config) {
	config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, entryKeyBoardTrace{})
}

type auditDeadlineTrace struct{ inserts, bounded *atomic.Int64 }

func (trace auditDeadlineTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "INSERT INTO organization_secret_audit") {
		trace.inserts.Add(1)
		if _, ok := ctx.Deadline(); ok {
			trace.bounded.Add(1)
		}
	}
	return ctx
}

func (auditDeadlineTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestSecretUseAuditIsNotBoundedByReadDeadline(t *testing.T) {
	t.Parallel()
	var inserts, bounded atomic.Int64
	repository, _ := projectRecordPostgresWithAdmin(t, 4, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, auditDeadlineTrace{inserts: &inserts, bounded: &bounded})
	})
	service := openLocalPostgresHubTestService(t, repository, Config{SecretKeys: secretTestKeys(t, "1", "1")})
	seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: "org_first", Name: "org_first", Slug: "org-first"})
	f := newNativeFixture(t, service, "org_first", "audit-deadline")
	now := time.Now().UTC()
	configureTestSlack(t, f, &now, true)
	inserts.Store(0)
	bounded.Store(0)
	value, err := f.service.readSlackWebhook(t.Context(), f.project.OrganizationID, "test")
	if err != nil || len(value) == 0 {
		t.Fatalf("webhook read: %v", err)
	}
	clear(value)
	if inserts.Load() != 1 || bounded.Load() != 0 {
		t.Fatalf("secret use audit inserts=%d bounded by read deadline=%d", inserts.Load(), bounded.Load())
	}
}
