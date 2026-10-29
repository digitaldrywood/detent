package hubserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeContentKey struct{}

type changeContent struct {
	mu       sync.Mutex
	call     entryauthority.ContentHandler
	register func(context.Context, string, attachment.ArchiveReference) error
	prepared map[string]attachment.ArchiveReference
}

func (s *Service) contentContext(ctx context.Context) context.Context {
	if ctx.Value(changeContentKey{}) != nil {
		return ctx
	}
	if s.config.Content != nil {
		return context.WithValue(ctx, changeContentKey{}, &changeContent{call: s.config.Content, register: s.registerChangeContent})
	}
	if s.config.Hosted == nil || s.config.Hosted.SharedEntry == nil || s.config.Hosted.SharedEntry.Content == nil {
		return ctx
	}
	return context.WithValue(ctx, changeContentKey{}, &changeContent{call: s.config.Hosted.SharedEntry.Content, register: s.registerChangeContent})
}

func (s *Service) changeContentMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.SetRequest(c.Request().WithContext(s.contentContext(c.Request().Context())))
		return next(c)
	}
}

func prepareChangeContent(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	content, ok := ctx.Value(changeContentKey{}).(*changeContent)
	if !ok {
		return attachment.ArchiveReference{}, errors.New("immutable content storage unavailable")
	}
	content.mu.Lock()
	defer content.mu.Unlock()
	digest := organization + "/" + artifact.Digest(data)
	if ref, found := content.prepared[digest]; found {
		return ref, nil
	}
	ref, compressed, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return ref, err
	}
	if err := content.register(ctx, organization, ref); err != nil {
		return ref, err
	}
	if _, err := content.call(ctx, entryauthority.ContentRequest{Organization: organization, Reference: ref, Data: compressed, Write: true}); err != nil {
		return ref, err
	}
	if content.prepared == nil {
		content.prepared = make(map[string]attachment.ArchiveReference)
	}
	content.prepared[digest] = ref
	return ref, nil
}

func readChangeContent(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Validate(organization) != nil {
		return nil, artifact.ErrIntegrity
	}
	return readRuntimeObject(ctx, nativeRuntimeObjectKey{kind: "content", organization: organization, reference: ref}, func() ([]byte, error) {
		return readChangeContentObject(ctx, organization, ref)
	})
}

func readChangeContentObject(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	content, ok := ctx.Value(changeContentKey{}).(*changeContent)
	if !ok {
		return nil, errors.New("immutable content storage unavailable")
	}
	compressed, err := content.call(ctx, entryauthority.ContentRequest{Organization: organization, Reference: ref})
	if err != nil {
		return nil, err
	}
	if int64(len(compressed)) != ref.CompressedSize || artifact.Digest(compressed) != ref.SHA256 {
		return nil, artifact.ErrIntegrity
	}
	input := bytes.NewReader(compressed)
	reader, err := gzip.NewReader(input)
	if err != nil {
		return nil, artifact.ErrIntegrity
	}
	reader.Multistream(false)
	data, err := io.ReadAll(io.LimitReader(reader, ref.UncompressedSize+1))
	if err = errors.Join(err, reader.Close()); err != nil || input.Len() != 0 || int64(len(data)) != ref.UncompressedSize {
		return nil, artifact.ErrIntegrity
	}
	return data, nil
}

func contentReference() attachment.ArchiveReference {
	return attachment.ArchiveReference{Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
}

func persistCommandOutput(ctx context.Context, tx *sql.Tx, organization string, version, review, role string, position int, command *gate.CommandResult) error {
	if command == nil || command.Output == "" {
		return nil
	}
	var ref attachment.ArchiveReference
	if prepared := preparedChangeMutation(ctx); prepared != nil {
		var found bool
		ref, found = prepared.outputs[fmt.Sprintf("%s.%d", role, position)]
		if !found {
			return errors.New("command output was not prepared")
		}
	} else {
		var err error
		ref, err = preparedChangeContent(ctx, organization, []byte(command.Output))
		if err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO change_command_outputs(version_id,review_id,role,position,organization_id,object_key,object_sha256,object_bytes,content_bytes) VALUES(?,?,?,?,?,?,?,?,?)`, version, review, role, position, organization, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize)
	if err == nil {
		command.Output = ""
	}
	return err
}

func hydrateChangeRecord(ctx context.Context, query nativeQueryer, value any) error {
	var version, review string
	var commands map[string][]gate.CommandResult
	switch record := value.(type) {
	case *tracker.ChangeVersion:
		version = record.ID
		var revision, scope string
		err := query.QueryRowContext(ctx, `SELECT v.policy_revision_id,c.organization_id||'/'||c.project_id FROM change_versions v JOIN change_requests c ON c.id=v.change_id WHERE v.id=?`, version).Scan(&revision, &scope)
		if err != nil {
			return err
		}
		if revision != "" {
			var raw string
			if err := query.QueryRowContext(ctx, `SELECT metadata_json FROM policy_revisions WHERE scope=? AND policy_id=?`, scope, revision).Scan(&raw); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(raw), &record.Policy); err != nil {
				return err
			}
			if err := record.Policy.Validate(); err != nil {
				return err
			}
			if record.Policy.ID != record.PolicyID || revision != record.PolicyID {
				return errors.New("version policy reference mismatch")
			}
			if record.ReviewPolicy.PolicyID == "" {
				record.ReviewPolicy.PolicyID = record.PolicyID
			}
			if record.ReviewPolicy.RequiredChecks == nil && len(record.Checks) > 0 {
				record.ReviewPolicy.RequiredChecks = make([]tracker.ChangeCheckSpec, len(record.Checks))
				for i, check := range record.Checks {
					record.ReviewPolicy.RequiredChecks[i] = check.ChangeCheckSpec
				}
			}
		}
		if record.Validation != nil {
			slots := make(map[string][]attributionSlot)
			commandAttributionSlots(slots, "validation", record.Validation)
			if err := readAttributionSlots(ctx, query, version, version, slots); err != nil {
				return err
			}
			commands = map[string][]gate.CommandResult{"validation": {*record.Validation}}
		}
	case *tracker.ChangeReview:
		version, review = record.VersionID, record.ID
		if err := readAttributionSlots(ctx, query, version, review, reviewAttributionSlots(record)); err != nil {
			return err
		}
		commands = make(map[string][]gate.CommandResult)
		if record.Validator != nil {
			commands["validator"] = record.Validator.Commands
		}
		if record.ImplementationReview != nil {
			commands["implementation_review"] = record.ImplementationReview.Commands
		}
	default:
		return nil
	}
	if len(commands) == 0 || claimPrepared(ctx) != nil || explanationRead(ctx) {
		return nil
	}
	rows, err := query.QueryContext(ctx, `SELECT role,position,organization_id,object_key,object_sha256,object_bytes,content_bytes FROM change_command_outputs WHERE version_id=? AND review_id=? ORDER BY role,position`, version, review)
	if err != nil {
		return err
	}
	defer rows.Close()
	type output struct {
		role, organization string
		position           int
		ref                attachment.ArchiveReference
	}
	var outputs []output
	for rows.Next() {
		row := output{ref: contentReference()}
		if err := rows.Scan(&row.role, &row.position, &row.organization, &row.ref.Key, &row.ref.SHA256, &row.ref.CompressedSize, &row.ref.UncompressedSize); err != nil {
			return err
		}
		outputs = append(outputs, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, row := range outputs {
		list := commands[row.role]
		if row.position >= len(list) {
			return errors.New("command output reference mismatch")
		}
		data, err := readChangeContent(ctx, row.organization, row.ref)
		if err != nil {
			return fmt.Errorf("read command output: %w", err)
		}
		list[row.position].Output = string(data)
	}
	if record, ok := value.(*tracker.ChangeVersion); ok && record.Validation != nil {
		validation := commands["validation"]
		if len(validation) != 1 {
			return errors.New("version validation reference mismatch")
		}
		*record.Validation = validation[0]
	}
	return nil
}

func compactChangeVersion(version tracker.ChangeVersion) (string, error) {
	raw, err := json.Marshal(version)
	if err != nil {
		return "", err
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil {
		return "", err
	}
	if record == nil {
		return "", errors.New("version snapshot must contain an object")
	}
	delete(record, "policy")
	var review map[string]json.RawMessage
	if err := json.Unmarshal(record["review_policy"], &review); err != nil {
		return "", err
	}
	delete(review, "policy_id")
	if len(version.Checks) > 0 {
		delete(review, "required_checks")
	}
	record["review_policy"], err = json.Marshal(review)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(record)
	return string(data), err
}

func preparedChangeContent(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	content, ok := ctx.Value(changeContentKey{}).(*changeContent)
	if !ok {
		return attachment.ArchiveReference{}, errors.New("immutable content storage unavailable")
	}
	content.mu.Lock()
	defer content.mu.Unlock()
	ref, found := content.prepared[organization+"/"+artifact.Digest(data)]
	if !found {
		return ref, errors.New("immutable content was not prepared before mutation")
	}
	return ref, nil
}

func (s *Service) registerChangeContent(ctx context.Context, organization string, ref attachment.ArchiveReference) error {
	return s.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		before, err := s.database.hostedConsumption(ctx, tx, now, "collaboration_bytes")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO change_content_objects(organization_id,object_sha256,object_key,object_bytes,content_bytes) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, organization, ref.SHA256, ref.Key, ref.CompressedSize, ref.UncompressedSize); err != nil {
			return err
		}
		return s.database.checkHostedGrowth(ctx, tx, before, now, false, "collaboration_bytes")
	})
}
