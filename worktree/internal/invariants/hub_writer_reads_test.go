package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	files, err := filepath.Glob(filepath.Join(repositoryRoot(t), "internal", "hubserver", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Hub source files to inspect")
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
