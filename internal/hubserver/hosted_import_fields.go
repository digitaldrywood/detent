package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const hostedImportRowsPerTick = 100

type hostedImportProjection struct {
	table, payload string
	columns        []string
	update         string
}

var hostedImportProjections = []hostedImportProjection{
	{"hosted_plans", "record_json", []string{"collaboration_bytes"}, `UPDATE hosted_plans SET collaboration_bytes=COALESCE(collaboration_bytes,?) WHERE rowid=? AND COALESCE(record_json,'null')=? AND (collaboration_bytes IS NULL)`},
	{"hosted_artifact_usage", "usage_json", []string{"retained_bytes", "reserved_bytes"}, `UPDATE hosted_artifact_usage SET retained_bytes=COALESCE(retained_bytes,?),reserved_bytes=COALESCE(reserved_bytes,?) WHERE rowid=? AND COALESCE(usage_json,'null')=? AND (retained_bytes IS NULL OR reserved_bytes IS NULL)`},
	{"hosted_sessions", "identity_json", []string{"identity_subject"}, `UPDATE hosted_sessions SET identity_subject=COALESCE(identity_subject,?) WHERE rowid=? AND COALESCE(identity_json,'null')=? AND (identity_subject IS NULL)`},
	{"artifact_services", "binding_json", []string{"binding_mode", "hosted_opt_in"}, `UPDATE artifact_services SET binding_mode=COALESCE(binding_mode,?),hosted_opt_in=COALESCE(hosted_opt_in,?) WHERE rowid=? AND COALESCE(binding_json,'null')=? AND (binding_mode IS NULL OR hosted_opt_in IS NULL)`},
	{"usage_cost_observations", "observation_json", []string{"voided"}, `UPDATE usage_cost_observations SET voided=COALESCE(voided,?) WHERE rowid=? AND COALESCE(observation_json,'null')=? AND (voided IS NULL)`},
	{"hosted_audit", "event", []string{"account_kind", "account_retry"}, `UPDATE hosted_audit SET account_kind=COALESCE(account_kind,?),account_retry=COALESCE(account_retry,?) WHERE rowid=? AND COALESCE(event,'null')=? AND (account_kind IS NULL OR account_retry IS NULL)`},
	{"github_import_records", "record_json", []string{"dependency_id", "provenance_external_id"}, `UPDATE github_import_records SET dependency_id=COALESCE(dependency_id,?),provenance_external_id=COALESCE(provenance_external_id,?) WHERE rowid=? AND COALESCE(record_json,'null')=? AND (dependency_id IS NULL OR provenance_external_id IS NULL)`},
	{"issues", "provenance_json", []string{"provenance_provider", "provenance_external_id"}, `UPDATE issues SET provenance_provider=COALESCE(provenance_provider,?),provenance_external_id=COALESCE(provenance_external_id,?) WHERE rowid=? AND COALESCE(provenance_json,'null')=? AND (provenance_provider IS NULL OR provenance_external_id IS NULL)`},
	{"onboarding_issue_intake", "request_json", []string{"hub_owned", "batch_status"}, `UPDATE onboarding_issue_intake SET hub_owned=COALESCE(hub_owned,?),batch_status=COALESCE(batch_status,?) WHERE rowid=? AND COALESCE(request_json,'null')=? AND (hub_owned IS NULL OR batch_status IS NULL)`},
	{"github_outbox", "desired_json", []string{"desired_issue_number", "desired_summary"}, `UPDATE github_outbox SET desired_issue_number=COALESCE(desired_issue_number,?),desired_summary=COALESCE(desired_summary,?) WHERE rowid=? AND COALESCE(desired_json,'null')=? AND (desired_issue_number IS NULL OR desired_summary IS NULL)`},
	{"runner_identities", "operations_json", []string{"operations_known"}, `UPDATE runner_identities SET operations_known=COALESCE(operations_known,?) WHERE rowid=? AND COALESCE(operations_json,'null')=? AND (operations_known IS NULL)`},
}

type outboxFields struct {
	IssueNumber int  `json:"issue_number"`
	Summary     bool `json:"summary"`
}

func decodeAccountFields(event string) accountReceipt {
	var value accountReceipt
	if strings.HasPrefix(strings.TrimSpace(event), "{") {
		if err := json.Unmarshal([]byte(event), &value); err != nil {
			return accountReceipt{}
		}
	}
	return value
}

type hostedImportFieldValues struct {
	columns      []any
	dependencyID string
	provenance   tracker.Provenance
	outbox       outboxFields
	voided       bool
	operations   []string
}

type coreGitHubOutboxImportValue struct {
	Source  postgres.NeutralImportRow   `json:"source"`
	Record  projectstore.GitHubOutbound `json:"record"`
	Desired json.RawMessage             `json:"desired"`
	Queued  bool                        `json:"queued"`
	Status  string                      `json:"status"`
}

func ImportCoreGitHubOutbox(ctx context.Context, target *postgres.Repository, source TenantCoreImportSource) (int64, error) {
	if source.CopyPath != "" || target == nil || source.BundlePath == "" || source.OrganizationID == "" || source.Objects == nil {
		return 0, postgres.ErrNeutralImport
	}
	if err := source.Catalog.Validate(); err != nil {
		return 0, err
	}
	var count int64
	content := eventstore.NewContent(source.Objects)
	err := postgres.ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *postgres.VerifiedNeutralImport) error {
		if _, exists := bundle.Schema()["github_outbox"]; !exists {
			return nil
		}
		for table, keys := range map[string][]string{"github_outbox": {"id"}, "issues": {"id"}, "projects": {"id"}, "repositories": {"id"}} {
			if err := bundle.ValidateCatalogTable(source.Catalog, bundle.Schema(), table, keys); err != nil {
				return err
			}
		}
		if err := target.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
			return err
		}
		var after []string
		for {
			rows, err := bundle.Page(ctx, "github_outbox", after, 100)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return nil
			}
			for _, raw := range rows {
				value, err := extractCoreGitHubOutbox(ctx, bundle, source, raw)
				if err != nil {
					return err
				}
				if value.Queued {
					value.Record.Payload, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: source.OrganizationID}, value.Desired)
					if err != nil {
						return err
					}
				} else {
					value.Record.Payload.OrganizationID = source.OrganizationID
				}
				receipt, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, bundle.Identity().SourceSHA256, "core-github-outbox", raw.Key[0], value)
				if err != nil {
					return err
				}
				if err := target.ImportCoreGitHubOutbound(ctx, value.Record, receipt); err != nil {
					return err
				}
				count++
			}
			after = rows[len(rows)-1].Key
		}
	})
	return count, err
}

func extractCoreGitHubOutbox(ctx context.Context, bundle *postgres.VerifiedNeutralImport, source TenantCoreImportSource, raw postgres.NeutralImportRow) (coreGitHubOutboxImportValue, error) {
	value := coreGitHubOutboxImportValue{Source: raw}
	row := &value.Record
	var sourceID, repositoryID int64
	var issueID sql.NullInt64
	var desired, created, updated string
	var targetNode, sourceBucket, lastAttempt, nextRetry, completed, processing, terminal, operator sql.NullString
	var number sql.NullInt64
	var summary sql.NullBool
	if err := raw.Decode(map[string]any{"id": &sourceID, "idempotency_key": &row.Key, "repository_id": &repositoryID, "issue_id": &issueID, "mutation_kind": &row.Kind, "desired_json": &desired, "desired_issue_number": &number, "desired_summary": &summary, "target_node_id": &targetNode, "status": &row.Status, "attempts": &row.Attempts, "last_attempt_at": &lastAttempt, "next_retry_at": &nextRetry, "completed_at": &completed, "processing_started_at": &processing, "created_at": &created, "updated_at": &updated, "coalesce_key": &sourceBucket, "terminal_error": &terminal, "operator_action": &operator}); err != nil {
		return value, err
	}
	if sourceID <= 0 || repositoryID <= 0 || !issueID.Valid || issueID.Int64 <= 0 || row.Key == "" || row.Kind != string(MutationWorkpad) || row.Attempts < 0 {
		return value, postgres.ErrNeutralImport
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(desired), &payload); err != nil {
		return value, err
	}
	if payload == nil {
		payload = make(map[string]json.RawMessage)
	}
	row.IssueNumber = number.Int64
	if !number.Valid && len(payload["issue_number"]) != 0 {
		if err := json.Unmarshal(payload["issue_number"], &row.IssueNumber); err != nil {
			return value, err
		}
	}
	isSummary := summary.Bool
	if !summary.Valid && len(payload["summary"]) != 0 {
		if err := json.Unmarshal(payload["summary"], &isSummary); err != nil {
			return value, err
		}
	}
	if row.IssueNumber < 0 {
		return value, postgres.ErrNeutralImport
	}
	var err error
	payload["issue_number"], err = json.Marshal(row.IssueNumber)
	if err != nil {
		return value, err
	}
	payload["summary"], err = json.Marshal(isSummary)
	if err != nil {
		return value, err
	}
	value.Desired, err = json.Marshal(payload)
	if err != nil {
		return value, err
	}
	issue, err := bundle.Lookup(ctx, "issues", []string{strconv.FormatInt(issueID.Int64, 10)})
	if err != nil {
		return value, err
	}
	var organization tracker.OrganizationID
	var issueRepository sql.NullInt64
	if err := issue.Decode(map[string]any{"organization_id": &organization, "native_id": &row.WorkItemID, "project_id": &row.ProjectID, "repository_id": &issueRepository}); err != nil {
		return value, err
	}
	if organization != source.OrganizationID || row.ProjectID == "" || row.WorkItemID == "" || issueRepository.Valid && issueRepository.Int64 != repositoryID {
		return value, corestore.ErrTenantMismatch
	}
	repository, err := bundle.Lookup(ctx, "repositories", []string{strconv.FormatInt(repositoryID, 10)})
	if err != nil {
		return value, err
	}
	var owner, name string
	if err := repository.Decode(map[string]any{"github_owner": &owner, "github_name": &name}); err != nil {
		return value, err
	}
	row.Repository = owner + "/" + name
	if _, _, valid := splitRepositoryFullName(row.Repository); !valid {
		return value, postgres.ErrNeutralImport
	}
	project, err := bundle.Lookup(ctx, "projects", []string{string(row.ProjectID)})
	if err != nil {
		return value, err
	}
	var profile, projection string
	if err := project.Decode(map[string]any{"organization_id": &organization, "profile": &profile, "github_projection": &projection}); err != nil {
		return value, err
	}
	if organization != source.OrganizationID || profile != "native" {
		return value, corestore.ErrTenantMismatch
	}
	row.TargetNodeID = targetNode.String
	row.CreatedAt, err = parseCoreImportTime(sql.NullString{String: created, Valid: true})
	if err != nil {
		return value, err
	}
	row.NextAttemptAt, err = parseCoreImportTime(sql.NullString{String: updated, Valid: true})
	if err != nil {
		return value, err
	}
	for _, timestamp := range []sql.NullString{lastAttempt, nextRetry, completed, processing} {
		if _, _, err := parseOptionalCoreImportTime(timestamp); err != nil {
			return value, err
		}
	}
	if nextRetry.Valid && nextRetry.String != "" {
		row.NextAttemptAt, err = parseCoreImportTime(nextRetry)
		if err != nil {
			return value, err
		}
	}
	row.Outcome = "unattempted"
	if row.Attempts > 0 || lastAttempt.Valid && lastAttempt.String != "" || processing.Valid && processing.String != "" || row.Status == outboxProcessing || row.Status == outboxRetrying || row.Status == outboxDeadLetter {
		row.Outcome = "uncertain"
	}
	switch row.Status {
	case outboxPending, outboxRetrying, outboxSuperseded:
	case outboxProcessing, outboxDeadLetter:
		row.Status = outboxRetrying
	case outboxCompleted:
		row.Outcome = "committed"
	default:
		return value, postgres.ErrNeutralImport
	}
	if row.IssueNumber == 0 {
		if row.Outcome != "unattempted" || isSummary && projection != "disabled" {
			return value, postgres.ErrNeutralImport
		}
		row.Status = outboxSuperseded
	} else {
		value.Queued = true
		if row.Outcome == "uncertain" {
			row.CoalesceKey = sourceBucket.String
		}
		if isSummary && row.CoalesceKey == "" {
			row.CoalesceKey = sourceBucket.String
			if row.CoalesceKey == "" {
				row.CoalesceKey = "summary:" + string(row.WorkItemID)
			}
		}
	}
	value.Status = row.Status
	return value, nil
}
