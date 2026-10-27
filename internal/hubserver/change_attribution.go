package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type attributionSlot struct {
	value    *string
	evidence **failureattribution.Evidence
}

func reviewAttributionSlots(review *tracker.ChangeReview) map[string][]attributionSlot {
	slots := make(map[string][]attributionSlot)
	for role, result := range map[string]*gate.ValidatorResult{"validator": review.Validator, "implementation_review": review.ImplementationReview} {
		if result == nil {
			continue
		}
		gate.NormalizeAttribution(result)
		list := []attributionSlot{{value: &result.Attribution}}
		for i := range result.Findings {
			f := &result.Findings[i]
			list = append(list, attributionSlot{&f.Attribution, &f.AttributionEvidence})
		}
		slots[role] = list
		for i := range result.Commands {
			commandAttributionSlots(slots, fmt.Sprintf("%s.command.%d", role, i), &result.Commands[i])
		}
	}
	if review.SecurityAudit != nil {
		run := &review.SecurityAudit.Run
		securityaudit.NormalizeAttribution(run)
		list := []attributionSlot{{value: &run.Attribution}}
		for i := range run.Findings {
			f := &run.Findings[i]
			list = append(list, attributionSlot{&f.Attribution, &f.AttributionEvidence})
		}
		slots["security_audit"] = list
	}
	return slots
}

func commandAttributionSlots(slots map[string][]attributionSlot, role string, command *gate.CommandResult) {
	if command.Attribution == "" {
		command.Attribution = failureattribution.Unknown
	}
	list := []attributionSlot{{value: &command.Attribution}}
	for i := range command.Findings {
		f := &command.Findings[i]
		if f.Attribution == "" {
			f.Attribution = failureattribution.Unknown
		}
		list = append(list, attributionSlot{&f.Attribution, &f.AttributionEvidence})
	}
	slots[role] = list
}

func storeAttributionSlots(ctx context.Context, tx *sql.Tx, version, record string, slots map[string][]attributionSlot) error {
	for role, list := range slots {
		for i, slot := range list {
			evidence := failureattribution.Evidence{BaseExitCode: -1}
			if slot.evidence != nil && *slot.evidence != nil {
				evidence = **slot.evidence
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO change_finding_attributions
(version_id,record_id,role,position,attribution,base_sha,head_sha,command,diagnostic,head_output_digest,base_output_digest,base_exit_code)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, version, record, role, i-1, *slot.value, evidence.BaseSHA, evidence.HeadSHA, evidence.Command, evidence.Diagnostic, evidence.HeadOutputDigest, evidence.BaseOutputDigest, evidence.BaseExitCode); err != nil {
				return fmt.Errorf("store finding attribution: %w", err)
			}
		}
	}
	return nil
}

func readAttributionSlots(ctx context.Context, query nativeQueryer, version, record string, slots map[string][]attributionSlot) error {
	rows, err := query.QueryContext(ctx, `SELECT role,position,attribution,base_sha,head_sha,command,diagnostic,head_output_digest,base_output_digest,base_exit_code FROM change_finding_attributions WHERE version_id=? AND record_id=?`, version, record)
	if err != nil {
		return fmt.Errorf("read finding attributions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var role, value string
		var position int
		var evidence failureattribution.Evidence
		if err := rows.Scan(&role, &position, &value, &evidence.BaseSHA, &evidence.HeadSHA, &evidence.Command, &evidence.Diagnostic, &evidence.HeadOutputDigest, &evidence.BaseOutputDigest, &evidence.BaseExitCode); err != nil {
			return err
		}
		list := slots[role]
		if position < -1 || position+1 >= len(list) {
			return errors.New("finding attribution position is invalid")
		}
		slot := list[position+1]
		if position >= 0 {
			var proof *failureattribution.Evidence
			if evidence.BaseSHA != "" {
				proof = &evidence
			}
			if !failureattribution.Valid(value, proof, evidence.BaseSHA, evidence.HeadSHA) {
				return errors.New("stored finding attribution evidence is invalid")
			}
		}
		*slot.value = value
		if slot.evidence != nil {
			*slot.evidence = nil
			if evidence.BaseSHA != "" {
				*slot.evidence = &evidence
			}
		}
	}
	return rows.Err()
}
