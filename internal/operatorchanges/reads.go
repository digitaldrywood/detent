package operatorchanges

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type readCursor struct {
	Offset  int64  `json:"offset,omitempty"`
	Digest  string `json:"digest,omitempty"`
	Change  string `json:"change"`
	Head    string `json:"head"`
	Version string `json:"version"`
	Section string `json:"section"`
	After   int64  `json:"after"`
	Through int64  `json:"through"`
}

func Read(ctx context.Context, query Source, args operatortool.ChangeArguments, result operatortool.ChangeResult) (operatortool.ChangeResult, error) {
	change, err := query.Change(ctx, args.ItemID, args.ChangeID)
	if err != nil {
		return result, err
	}
	query, err = query.Archive(ctx, args.ItemID, args.ChangeID)
	if err != nil {
		return result, err
	}
	result.Archive = query.ArchiveDescriptor(ctx)
	selection := readCursor{Change: change.ID, Head: change.CurrentVersion, Version: args.VersionID, Section: args.Section}
	if args.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(args.Cursor)
		if err != nil || operatortool.DecodeArguments(raw, &selection) != nil || selection.Offset < 0 || selection.Offset > 0 && (selection.Section != "reviews" || len(selection.Digest) != 64) || selection.After < 0 || selection.Through < selection.After || selection.Change != change.ID || selection.Version == "" && change.CurrentVersion != "" || !validChangeReadSection(selection.Section) || selection.Section == "current" || args.Section != "" && args.Section != selection.Section || args.VersionID != "" && args.VersionID != selection.Version {
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
	detail, err := query.Detail(ctx, change, result.GeneratedAt)
	if err != nil {
		return result, err
	}
	detail.SourceIssues, err = query.SourceIssues(ctx, change.ID)
	if err != nil {
		return result, err
	}
	if selection.Version != "" {
		version, err := query.Version(ctx, change.ID, selection.Version)
		if err != nil {
			return result, err
		}
		if selection.Section != "versions" {
			detail.Versions = []tracker.ChangeVersion{BoundedVersion(version)}
			push, found, err := query.Publication(ctx, args.ItemID, version)
			if err != nil {
				return result, err
			}
			if found {
				detail.PublicationPushes = append(detail.PublicationPushes, push)
			}
		}
	}
	result.Detail = &detail
	result.Read = &operatortool.ChangeRead{VersionID: selection.Version, PolicyDetailsOmitted: true, Complete: true, Sections: map[string]operatortool.ChangeReadPage{}}
	sections := []string{selection.Section}
	if selection.Section == "current" {
		sections = []string{"versions", "reviews", "checks", "discussion"}
	}
	cursors := map[string]readCursor{}
	for _, section := range []string{"versions", "reviews", "checks", "discussion"} {
		cursor := selection
		cursor.Section = section
		if args.Cursor == "" || section != selection.Section {
			cursor.After, cursor.Offset, cursor.Digest = 0, 0, ""
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
		next, err := encodeCursor(cursor)
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

func encodeCursor(cursor readCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw), err
}

func readChangeSection(ctx context.Context, query Source, args operatortool.ChangeArguments, result *operatortool.ChangeResult, cursor readCursor, budget int, single, omitHistory bool) (operatortool.ChangeReadPage, error) {
	statement := "SELECT sequence, COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_evidence WHERE change_id=? AND kind=? AND sequence>? AND sequence<=? AND version_id=? ORDER BY sequence LIMIT ?"
	parameters := []any{cursor.Change, cursor.Section[:len(cursor.Section)-1], cursor.After, cursor.Through, cursor.Version, changeLimit(args) + 1}
	if cursor.Section == "discussion" {
		statement = "SELECT sequence, COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_evidence WHERE change_id=? AND kind='discussion' AND sequence>? AND sequence<=? AND (version_id=? OR version_id IS NULL OR version_id='') ORDER BY sequence LIMIT ?"
		parameters = []any{cursor.Change, cursor.After, cursor.Through, cursor.Version, changeLimit(args) + 1}
	}
	if cursor.Section == "versions" {
		statement = "SELECT number, COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_versions WHERE change_id=? AND number>? AND number<=? AND id!=? ORDER BY number LIMIT ?"
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
	if cursor.Offset > 0 && len(records) == 0 {
		return operatortool.ChangeReadPage{}, mutation.ErrConflict
	}
	used := 0
	for i, row := range records {
		if omitHistory || i == changeLimit(args) {
			break
		}
		resolved, err := query.Resolve(ctx, row.raw)
		if err != nil {
			return operatortool.ChangeReadPage{}, err
		}
		row.raw = resolved
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
			version, err = query.Version(ctx, cursor.Change, version.ID)
			if err != nil {
				return operatortool.ChangeReadPage{}, err
			}
			detail.Versions = append(detail.Versions, BoundedVersion(version))
			push, found, err := query.Publication(ctx, args.ItemID, version)
			if err != nil {
				return operatortool.ChangeReadPage{}, err
			}
			if found {
				detail.PublicationPushes = append(detail.PublicationPushes, push)
			}
		case "reviews":
			var review tracker.ChangeReview
			if err := json.Unmarshal(row.raw, &review); err != nil {
				return operatortool.ChangeReadPage{}, err
			}
			if err := query.Hydrate(ctx, &review); err != nil {
				return operatortool.ChangeReadPage{}, err
			}
			row.raw, err = json.Marshal(review)
			if err != nil {
				return operatortool.ChangeReadPage{}, err
			}
			review.Validator = BoundedReview(review.Validator)
			review.ImplementationReview = BoundedReview(review.ImplementationReview)
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
		if used+size > budget || cursor.Offset > 0 {
			*detail = before
			if single && i == 0 && cursor.Section == "reviews" {
				fragment := &tracker.NativeRecoveryPage{NativeReference: tracker.NativeReference{OrganizationID: detail.Change.OrganizationID, ProjectID: detail.Change.ProjectID, WorkItemID: detail.Change.WorkItemID}, Section: cursor.Section, RecordID: strconv.FormatInt(row.sequence, 10), VersionID: cursor.Version}
				if err := query.Fragment(fragment, row.raw, cursor.Offset, cursor.Digest); err != nil {
					return operatortool.ChangeReadPage{}, err
				}
				result.Fragment = fragment
				encoded, err := json.Marshal(result)
				if err != nil {
					return operatortool.ChangeReadPage{}, err
				}
				if excess := len(encoded) + 512 - operatortool.MaxResultBytes; excess > 0 {
					keep := len(fragment.Data) - (excess+3)*3/4
					if keep <= 0 {
						return operatortool.ChangeReadPage{}, operatortool.ChangeRecordTooLarge()
					}
					fragment.Data = fragment.Data[:keep]
				}
				cursor.Offset, cursor.Digest = fragment.Offset+int64(len(fragment.Data)), fragment.SHA256
				if cursor.Offset == fragment.TotalBytes {
					cursor.After, cursor.Offset, cursor.Digest = row.sequence, 0, ""
				}
			} else if single && i == 0 || size > operatortool.MaxResultBytes && i == 0 && cursor.Section != "reviews" {
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
	next, err := encodeCursor(cursor)
	return operatortool.ChangeReadPage{NextCursor: next}, err
}

func BoundedReview(result *gate.ValidatorResult) *gate.ValidatorResult {
	if result == nil {
		return nil
	}
	bounded := *result
	bounded.Commands = slices.Clone(bounded.Commands)
	for i := range bounded.Commands {
		bounded.Commands[i] = *BoundedCommand(&bounded.Commands[i])
	}
	return &bounded
}

func BoundedCommand(command *gate.CommandResult) *gate.CommandResult {
	if command == nil {
		return nil
	}
	result := tracker.PublicGateResult(*command)
	return &result
}

func BoundedLanding(landing *tracker.NativeLandingReceipt) *tracker.NativeLandingReceipt {
	if landing == nil {
		return nil
	}
	result := *landing
	result.Gate = BoundedCommand(result.Gate)
	return &result
}

func BoundedVersion(version tracker.ChangeVersion) tracker.ChangeVersion {
	if version.Policy.Authored != nil {
		authored := *version.Policy.Authored
		authored.Files = nil
		version.Policy.Authored = &authored
	}
	version.Policy.Configuration = nil
	version.Policy.Workflow = nil
	version.Validation = BoundedCommand(version.Validation)
	version.Landing = BoundedLanding(version.Landing)
	return version
}

func changeLimit(args operatortool.ChangeArguments) int {
	if args.Limit == 0 {
		return 100
	}
	return args.Limit
}

type Source interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	Change(context.Context, string, string) (tracker.ChangeRequest, error)
	Archive(context.Context, string, string) (Source, error)
	ArchiveDescriptor(context.Context) *tracker.ArchiveDescriptor
	Detail(context.Context, tracker.ChangeRequest, time.Time) (tracker.ChangeDetail, error)
	SourceIssues(context.Context, string) ([]tracker.ExternalReference, error)
	Version(context.Context, string, string) (tracker.ChangeVersion, error)
	Publication(context.Context, string, tracker.ChangeVersion) (tracker.ChangePublicationPush, bool, error)
	Resolve(context.Context, []byte) ([]byte, error)
	Hydrate(context.Context, any) error
	Fragment(*tracker.NativeRecoveryPage, []byte, int64, string) error
}
