package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

func (s *sqliteStore) RecordValidatorVerdict(ctx context.Context, attrs ValidatorVerdict) error {
	projectID := strings.TrimSpace(attrs.ProjectID)
	if projectID == "" {
		return errors.New("project_id is required")
	}
	issueID := strings.TrimSpace(attrs.IssueID)
	if issueID == "" {
		return errors.New("issue_id is required")
	}
	headSHA := strings.TrimSpace(attrs.HeadSHA)
	if headSHA == "" {
		return errors.New("head_sha is required")
	}
	recordedAt, err := requiredTimestamp("recorded_at", attrs.RecordedAt)
	if err != nil {
		return err
	}
	updatedAtValue := attrs.UpdatedAt
	if updatedAtValue.IsZero() {
		updatedAtValue = attrs.RecordedAt
	}
	updatedAt, err := requiredTimestamp("updated_at", updatedAtValue)
	if err != nil {
		return err
	}
	findingsJSON, err := validatorFindingsJSON(attrs.Findings)
	if err != nil {
		return err
	}

	if _, err := s.queries.UpsertValidatorVerdict(ctx, sqlc.UpsertValidatorVerdictParams{
		ProjectID:    projectID,
		IssueID:      issueID,
		HeadSha:      headSHA,
		Identifier:   nullString(attrs.Identifier),
		IssueURL:     nullString(attrs.IssueURL),
		PrNumber:     nullOptionalInt64(attrs.PRNumber),
		Submitted:    boolInt64(attrs.Submitted),
		Verdict:      strings.TrimSpace(attrs.Verdict),
		Score:        nonNegativeFloat(attrs.Score),
		Summary:      nullString(attrs.Summary),
		FindingsJson: findingsJSON,
		Commented:    boolInt64(attrs.Commented),
		RecordedAt:   recordedAt,
		UpdatedAt:    updatedAt,
	}); err != nil {
		return fmt.Errorf("recording validator verdict: %w", err)
	}
	return nil
}

func (s *sqliteStore) ValidatorVerdict(ctx context.Context, key ValidatorVerdictKey) (ValidatorVerdict, error) {
	projectID := strings.TrimSpace(key.ProjectID)
	if projectID == "" {
		return ValidatorVerdict{}, errors.New("project_id is required")
	}
	issueID := strings.TrimSpace(key.IssueID)
	if issueID == "" {
		return ValidatorVerdict{}, errors.New("issue_id is required")
	}
	headSHA := strings.TrimSpace(key.HeadSHA)
	if headSHA == "" {
		return ValidatorVerdict{}, errors.New("head_sha is required")
	}

	row, err := s.queries.GetValidatorVerdict(ctx, sqlc.GetValidatorVerdictParams{
		ProjectID: projectID,
		IssueID:   issueID,
		HeadSha:   headSHA,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ValidatorVerdict{}, ErrNotFound
		}
		return ValidatorVerdict{}, fmt.Errorf("reading validator verdict: %w", err)
	}
	return validatorVerdictFromRow(row)
}

func (s *sqliteStore) ListValidatorVerdicts(ctx context.Context, query ValidatorVerdictQuery) ([]ValidatorVerdict, error) {
	from, err := optionalTimestamp("from", query.From)
	if err != nil {
		return nil, err
	}
	to, err := optionalTimestamp("to", query.To)
	if err != nil {
		return nil, err
	}
	if from.Valid && to.Valid && from.String >= to.String {
		return nil, errors.New("from must be before to")
	}

	rows, err := s.queries.ListValidatorVerdicts(ctx, sqlc.ListValidatorVerdictsParams{
		FilterProjectID: strings.TrimSpace(query.ProjectID),
		FromTime:        from,
		ToTime:          to,
	})
	if err != nil {
		return nil, fmt.Errorf("listing validator verdicts: %w", err)
	}

	verdicts := make([]ValidatorVerdict, 0, len(rows))
	for _, row := range rows {
		verdict, err := validatorVerdictFromRow(row)
		if err != nil {
			return nil, err
		}
		verdicts = append(verdicts, verdict)
	}
	return verdicts, nil
}

func (s *sqliteStore) MarkValidatorVerdictCommented(ctx context.Context, key ValidatorVerdictKey, at time.Time) error {
	projectID := strings.TrimSpace(key.ProjectID)
	if projectID == "" {
		return errors.New("project_id is required")
	}
	issueID := strings.TrimSpace(key.IssueID)
	if issueID == "" {
		return errors.New("issue_id is required")
	}
	headSHA := strings.TrimSpace(key.HeadSHA)
	if headSHA == "" {
		return errors.New("head_sha is required")
	}
	updatedAt, err := requiredTimestamp("updated_at", at)
	if err != nil {
		return err
	}

	affected, err := s.queries.MarkValidatorVerdictCommented(ctx, sqlc.MarkValidatorVerdictCommentedParams{
		UpdatedAt: updatedAt,
		ProjectID: projectID,
		IssueID:   issueID,
		HeadSha:   headSHA,
	})
	if err != nil {
		return fmt.Errorf("marking validator verdict commented: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func validatorVerdictFromRow(row sqlc.ValidatorVerdict) (ValidatorVerdict, error) {
	recordedAt, err := parseTimestamp("recorded_at", row.RecordedAt)
	if err != nil {
		return ValidatorVerdict{}, err
	}
	updatedAt, err := parseTimestamp("updated_at", row.UpdatedAt)
	if err != nil {
		return ValidatorVerdict{}, err
	}
	findings, err := validatorFindingsFromJSON(row.FindingsJson)
	if err != nil {
		return ValidatorVerdict{}, err
	}

	return ValidatorVerdict{
		ProjectID:  row.ProjectID,
		IssueID:    row.IssueID,
		HeadSHA:    row.HeadSha,
		Identifier: row.Identifier.String,
		IssueURL:   row.IssueURL.String,
		PRNumber:   optionalInt64Pointer(row.PrNumber),
		Submitted:  row.Submitted != 0,
		Verdict:    row.Verdict,
		Score:      nonNegativeFloat(row.Score),
		Summary:    row.Summary.String,
		Findings:   findings,
		Commented:  row.Commented != 0,
		RecordedAt: recordedAt.UTC(),
		UpdatedAt:  updatedAt.UTC(),
	}, nil
}

func validatorFindingsJSON(findings []ValidatorFinding) (string, error) {
	if findings == nil {
		findings = []ValidatorFinding{}
	}
	data, err := json.Marshal(findings)
	if err != nil {
		return "", fmt.Errorf("encoding validator findings: %w", err)
	}
	return string(data), nil
}

func validatorFindingsFromJSON(value string) ([]ValidatorFinding, error) {
	if strings.TrimSpace(value) == "" {
		return []ValidatorFinding{}, nil
	}
	var findings []ValidatorFinding
	if err := json.Unmarshal([]byte(value), &findings); err != nil {
		return nil, fmt.Errorf("decoding validator findings: %w", err)
	}
	if findings == nil {
		return []ValidatorFinding{}, nil
	}
	return findings, nil
}
