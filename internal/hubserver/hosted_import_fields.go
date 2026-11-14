package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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

type coreGitHubOutboxImportValue struct {
	Source  neutralimport.Row           `json:"source"`
	Record  projectstore.GitHubOutbound `json:"record"`
	Desired json.RawMessage             `json:"desired"`
	Queued  bool                        `json:"queued"`
	Status  string                      `json:"status"`
}

func ImportCoreGitHubOutbox(ctx context.Context, target *postgres.Repository, source TenantCoreImportSource) (int64, error) {
	if source.CopyPath != "" || target == nil || source.BundlePath == "" || source.OrganizationID == "" || source.Objects == nil {
		return 0, neutralimport.ErrInvalid
	}
	if err := source.Catalog.Validate(); err != nil {
		return 0, err
	}
	var count int64
	content := eventstore.NewContent(source.Objects)
	err := neutralimport.Read(ctx, source.BundlePath, source.OrganizationID, func(bundle *neutralimport.Bundle) error {
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

func extractCoreGitHubOutbox(ctx context.Context, bundle *neutralimport.Bundle, source TenantCoreImportSource, raw neutralimport.Row) (coreGitHubOutboxImportValue, error) {
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
		return value, neutralimport.ErrInvalid
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
		return value, neutralimport.ErrInvalid
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
		return value, neutralimport.ErrInvalid
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
	row.CreatedAt, err = coreimport.ParseImportTime(sql.NullString{String: created, Valid: true})
	if err != nil {
		return value, err
	}
	row.NextAttemptAt, err = coreimport.ParseImportTime(sql.NullString{String: updated, Valid: true})
	if err != nil {
		return value, err
	}
	for _, timestamp := range []sql.NullString{lastAttempt, nextRetry, completed, processing} {
		if _, _, err := coreimport.ParseOptionalImportTime(timestamp); err != nil {
			return value, err
		}
	}
	if nextRetry.Valid && nextRetry.String != "" {
		row.NextAttemptAt, err = coreimport.ParseImportTime(nextRetry)
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
		return value, neutralimport.ErrInvalid
	}
	if row.IssueNumber == 0 {
		if row.Outcome != "unattempted" || isSummary && projection != "disabled" {
			return value, neutralimport.ErrInvalid
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
