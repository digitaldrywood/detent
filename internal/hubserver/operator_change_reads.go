package hubserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeReadCursor struct {
	Change  string `json:"change"`
	Head    string `json:"head"`
	Version string `json:"version"`
	Section string `json:"section"`
	After   int64  `json:"after"`
	Through int64  `json:"through"`
}

func readBoundedChange(ctx context.Context, query nativeQueryer, scope nativeScope, args operatortool.ChangeArguments, result operatortool.ChangeResult) (operatortool.ChangeResult, error) {
	change, err := readChange(ctx, query, scope, args.ItemID, args.ChangeID)
	if err != nil {
		return result, err
	}
	selection := changeReadCursor{Change: change.ID, Head: change.CurrentVersion, Version: args.VersionID, Section: args.Section}
	if args.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(args.Cursor)
		if err != nil || operatortool.DecodeArguments(raw, &selection) != nil || selection.After < 0 || selection.Through < selection.After || selection.Change != change.ID || selection.Version == "" && change.CurrentVersion != "" || !validChangeReadSection(selection.Section) || selection.Section == "current" || args.Section != "" && args.Section != selection.Section || args.VersionID != "" && args.VersionID != selection.Version {
			return result, operatortool.ErrInvalidArguments
		}
		if selection.Head != change.CurrentVersion {
			return result, mutation.ErrConflict
		}
	}
	if selection.Version == "" {
		selection.Version = change.CurrentVersion
	}
	if selection.Section == "" {
		selection.Section = "current"
	}
	if !validChangeReadSection(selection.Section) {
		return result, operatortool.ErrInvalidArguments
	}
	detail, err := readCurrentChangeDetail(ctx, query, scope, change, result.GeneratedAt)
	if err != nil {
		return result, err
	}
	detail.SourceIssues, err = readChangeSourceIssues(ctx, query, scope, change.ID)
	if err != nil {
		return result, err
	}
	if selection.Version != "" {
		version, err := readChangeVersion(ctx, query, change.ID, selection.Version)
		if err != nil {
			return result, err
		}
		if selection.Section != "versions" {
			detail.Versions = []tracker.ChangeVersion{version}
		}
	}
	result.Detail = &detail
	result.ValidationAudit, err = readValidationAudit(ctx, query, scope, "", "", change.ID, "")
	if err != nil {
		return result, err
	}
	result.Read = &operatortool.ChangeRead{VersionID: selection.Version, Complete: true, Sections: map[string]operatortool.ChangeReadPage{}}
	sections := []string{selection.Section}
	if selection.Section == "current" {
		sections = []string{"versions", "reviews", "checks", "discussion"}
	}
	cursors := map[string]changeReadCursor{}
	for _, section := range []string{"versions", "reviews", "checks", "discussion"} {
		cursor := selection
		cursor.Section = section
		if args.Cursor == "" || section != selection.Section {
			cursor.After = 0
			if section == "versions" {
				err = query.QueryRowContext(ctx, "SELECT COALESCE(MAX(number), 0) FROM change_versions WHERE change_id=?", change.ID).Scan(&cursor.Through)
			} else {
				err = query.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence), 0) FROM change_evidence WHERE change_id=?", change.ID).Scan(&cursor.Through)
			}
			if err != nil {
				return result, err
			}
		}
		cursors[section] = cursor
		next, err := encodeChangeReadCursor(cursor)
		if err != nil {
			return result, err
		}
		result.Read.Sections[section] = operatortool.ChangeReadPage{NextCursor: next}
	}
	base, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	if len(base) > operatortool.MaxResultBytes {
		return result, operatortool.ChangeRecordTooLarge()
	}
	budget := (operatortool.MaxResultBytes - len(base) - 512) / len(sections)
	for _, section := range sections {
		page, err := readChangeSection(ctx, query, args, &result, cursors[section], budget, len(sections) == 1, selection.Section == "current" && section == "versions")
		if err != nil {
			return result, err
		}
		result.Read.Sections[section] = page
	}
	for _, page := range result.Read.Sections {
		result.Read.Complete = result.Read.Complete && page.Complete
	}
	if _, err := operatortool.BoundedChangeResult(result); err != nil {
		return result, err
	}
	return result, nil
}

func validChangeReadSection(section string) bool {
	return section == "current" || section == "versions" || section == "reviews" || section == "checks" || section == "discussion"
}

func encodeChangeReadCursor(cursor changeReadCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw), err
}

func readChangeSection(ctx context.Context, query nativeQueryer, args operatortool.ChangeArguments, result *operatortool.ChangeResult, cursor changeReadCursor, budget int, single, omitHistory bool) (operatortool.ChangeReadPage, error) {
	statement := "SELECT sequence, record_json FROM change_evidence WHERE change_id=? AND kind=? AND sequence>? AND sequence<=? AND version_id=? ORDER BY sequence LIMIT ?"
	parameters := []any{cursor.Change, cursor.Section[:len(cursor.Section)-1], cursor.After, cursor.Through, cursor.Version, changeLimit(args) + 1}
	if cursor.Section == "discussion" {
		statement = "SELECT sequence, record_json FROM change_evidence WHERE change_id=? AND kind='discussion' AND sequence>? AND sequence<=? AND (version_id=? OR version_id IS NULL OR version_id='') ORDER BY sequence LIMIT ?"
		parameters = []any{cursor.Change, cursor.After, cursor.Through, cursor.Version, changeLimit(args) + 1}
	}
	if cursor.Section == "versions" {
		statement = "SELECT number, record_json FROM change_versions WHERE change_id=? AND number>? AND number<=? AND id!=? ORDER BY number LIMIT ?"
		parameters = []any{cursor.Change, cursor.After, cursor.Through, cursor.Version, changeLimit(args) + 1}
	}
	rows, err := query.QueryContext(ctx, statement, parameters...)
	if err != nil {
		return operatortool.ChangeReadPage{}, err
	}
	defer rows.Close()
	type record struct {
		sequence int64
		raw      []byte
	}
	var records []record
	for rows.Next() {
		var row record
		if err := rows.Scan(&row.sequence, &row.raw); err != nil {
			return operatortool.ChangeReadPage{}, err
		}
		records = append(records, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return operatortool.ChangeReadPage{}, err
	}
	used := 0
	for i, row := range records {
		if omitHistory || i == changeLimit(args) {
			break
		}
		detail := result.Detail
		before := *detail
		previous, err := json.Marshal(result)
		if err != nil {
			return operatortool.ChangeReadPage{}, err
		}
		switch cursor.Section {
		case "versions":
			var version tracker.ChangeVersion
			if err := json.Unmarshal(row.raw, &version); err != nil {
				return operatortool.ChangeReadPage{}, err
			}
			version, err = readChangeVersion(ctx, query, cursor.Change, version.ID)
			if err != nil {
				return operatortool.ChangeReadPage{}, err
			}
			detail.Versions = append(detail.Versions, version)
		case "reviews":
			var review tracker.ChangeReview
			if err := json.Unmarshal(row.raw, &review); err != nil {
				return operatortool.ChangeReadPage{}, err
			}
			detail.Reviews = append(detail.Reviews, review)
		case "checks":
			var check tracker.ChangeCheck
			if err := json.Unmarshal(row.raw, &check); err != nil {
				return operatortool.ChangeReadPage{}, err
			}
			detail.Checks = append(detail.Checks, check)
		case "discussion":
			var discussion tracker.ChangeDiscussion
			if err := json.Unmarshal(row.raw, &discussion); err != nil {
				return operatortool.ChangeReadPage{}, err
			}
			detail.Discussion = append(detail.Discussion, discussion)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return operatortool.ChangeReadPage{}, err
		}
		size := len(encoded) - len(previous)
		if used+size > budget {
			*detail = before
			if size > operatortool.MaxResultBytes && i == 0 {
				return operatortool.ChangeReadPage{}, operatortool.ChangeRecordTooLarge()
			}
			if single && i == 0 {
				return operatortool.ChangeReadPage{}, operatortool.ChangeRecordTooLarge()
			}
			break
		}
		used += size
		cursor.After = row.sequence
	}
	complete := len(records) == 0 || cursor.After == records[len(records)-1].sequence
	if complete {
		return operatortool.ChangeReadPage{Complete: true}, nil
	}
	next, err := encodeChangeReadCursor(cursor)
	return operatortool.ChangeReadPage{NextCursor: next}, err
}
