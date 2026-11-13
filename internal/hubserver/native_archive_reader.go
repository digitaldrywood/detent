package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeArchiveContextKey struct{}

type nativeArchiveRead struct {
	service     *Service
	scope       hubscope.NativeScope
	presign     bool
	webResponse bool
	descriptor  *tracker.ArchiveDescriptor
	section     *attachment.ArchiveSection
	first       int64
	last        int64
	err         error
}

func (s *Service) archiveContext(ctx context.Context, scope hubscope.NativeScope, presign bool) context.Context {
	if ctx.Value(nativeArchiveContextKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, nativeArchiveContextKey{}, &nativeArchiveRead{service: s, scope: scope, presign: presign})
}

func (s *Service) archiveRequestContext(c echo.Context, scope hubscope.NativeScope) context.Context {
	ctx := s.archiveContext(c.Request().Context(), scope, c.QueryParam("archive_url") == "true")
	if state, ok := ctx.Value(nativeArchiveContextKey{}).(*nativeArchiveRead); ok {
		state.webResponse = c.Request().Header.Get("X-Detent-Archive-Read") == "1"
	}
	return ctx
}

func nativeArchiveDescriptor(ctx context.Context) *tracker.ArchiveDescriptor {
	if state, ok := ctx.Value(nativeArchiveContextKey{}).(*nativeArchiveRead); ok && !state.webResponse {
		return state.descriptor
	}
	return nil
}

func withNativeArchive(raw []byte, descriptor *tracker.ArchiveDescriptor) ([]byte, error) {
	if descriptor == nil {
		return raw, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("archive response must be an object")
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		return nil, err
	}
	object["archive"] = encoded
	return json.Marshal(object)
}

func nativeArchiveJSON(c echo.Context, status int, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	raw, err = withNativeArchive(raw, nativeArchiveDescriptor(c.Request().Context()))
	if err != nil {
		return err
	}
	return c.JSONBlob(status, raw)
}

func (r *nativeArchiveRead) load(ctx context.Context, item, kind, subject string, sequence int64, id string, first, last int64, ref attachment.ArchiveReference) (attachment.ArchiveSection, bool, error) {
	s, scope := r.service, r.scope
	if r.descriptor != nil {
		if r.section == nil || r.section.SectionID != id || r.section.WorkItemID != item || r.section.ProjectID != string(scope.Project) || r.section.OrganizationID != string(scope.Organization) || r.descriptor.SHA256 != ref.SHA256 {
			if r.err != nil {
				return attachment.ArchiveSection{}, false, r.err
			}
			return attachment.ArchiveSection{}, false, &hubapi.NativeArchiveError{Archive: &tracker.ArchiveDescriptor{Availability: "section_required", SectionIDs: []string{id}}}
		}
		return *r.section, true, r.err
	}
	r.descriptor = &tracker.ArchiveDescriptor{Availability: "unavailable", SectionIDs: []string{id}, SHA256: ref.SHA256}
	fail := func(availability string) (attachment.ArchiveSection, bool, error) {
		r.descriptor.Availability = availability
		r.err = &hubapi.NativeArchiveError{Archive: r.descriptor}
		return attachment.ArchiveSection{}, false, r.err
	}
	if ref.Validate(string(scope.Organization)) != nil || ref.UncompressedSize > attachment.MaxSectionBytes || ref.SchemaVersion != 1 || id == "" || (sequence > 0 && kind != "conversation" && sequence > last) {
		return fail("invalid")
	}
	reader := s.config.ArchiveReader
	if reader == nil && s.hostedShared() {
		reader = s.config.Hosted.SharedEntry.ReadArchive
	}
	if reader == nil {
		return fail("unavailable")
	}
	unrestricted := scope.Credential.Scope == hubapi.APIScopeAdmin && !scope.Credential.NativeOnly && scope.Credential.Hosted == nil
	unrestricted = unrestricted || scope.Credential.Hosted != nil && scope.Credential.EntryKey == nil && scope.Credential.HostedKeyScope == "" && (scope.Credential.HostedRole == "owner" || scope.Credential.HostedRole == "admin")
	presign := r.presign && unrestricted
	response, err := hubcontent.ReadRuntimeObject(ctx, hubcontent.NativeRuntimeObjectKey{Kind: "archive", Organization: string(scope.Organization), Reference: ref, Presign: presign}, func() (entryauthority.ArchiveResponse, error) {
		return reader(ctx, ref, presign)
	})
	var hydration *hubcontent.NativeRuntimeHydration
	if errors.As(err, &hydration) {
		r.descriptor = nil
		return attachment.ArchiveSection{}, false, err
	}
	if err != nil {
		return fail("unavailable")
	}
	section, err := attachment.DecodeArchiveSection(string(scope.Organization), ref, response.Data)
	if err != nil || section.ProjectID != string(scope.Project) || section.WorkItemID != item || section.SectionID != id {
		return fail("invalid")
	}
	if err := validateNativeArchiveSection(section, kind, subject, first, last); err != nil {
		return fail("invalid")
	}
	r.section = &section
	r.first = first
	r.last = last
	r.descriptor.Availability = "available"
	if presign && response.Access != nil && response.Access.ArchiveReference == ref && response.Access.ExpiresAt.After(s.config.now()) && !response.Access.ExpiresAt.After(s.config.now().Add(attachment.ArchiveReadLifetime)) {
		r.descriptor.URL, r.descriptor.ExpiresAt = response.Access.URL, &response.Access.ExpiresAt
	}
	return *r.section, true, nil
}

func nativeArchiveTables(kind string) []string {
	switch kind {
	case "run":
		return []string{"native_attempts", "native_attempt_events", "attempt_diffs", "attempt_diff_files"}
	case "change":
		return []string{"change_versions", "change_evidence"}
	case "revision":
		return []string{"collaboration_versions"}
	case "conversation":
		return []string{"conversation_messages", "conversation_events"}
	case "history":
		return []string{"collaboration_events"}
	default:
		return nil
	}
}

func validateNativeArchiveSection(section attachment.ArchiveSection, kind, subject string, first, last int64) error {
	seen := map[string]bool{}
	parents := map[string]bool{}
	records := 0
	for _, table := range section.Tables {
		if !slices.Contains(nativeArchiveTables(kind), table.Name) || seen[table.Name] || len(table.Columns) == 0 || table.Columns[0] != "rowid" {
			return attachment.ErrInvalid
		}
		seen[table.Name] = true
		identities := map[int64]bool{}
		for _, row := range table.Rows {
			records++
			if len(row) != len(table.Columns) {
				return attachment.ErrInvalid
			}
			var rowID int64
			if json.Unmarshal(row[0], &rowID) != nil || rowID <= 0 || identities[rowID] {
				return attachment.ErrInvalid
			}
			identities[rowID] = true
			sequenceColumn := ""
			if table.Name == "collaboration_versions" {
				sequenceColumn = "revision"
			}
			if table.Name == "conversation_messages" {
				sequenceColumn = "seq"
			}
			if table.Name == "collaboration_events" {
				sequenceColumn = "sequence"
			}
			if sequenceColumn != "" {
				index := slices.Index(table.Columns, sequenceColumn)
				var sequence int64
				if index < 0 || json.Unmarshal(row[index], &sequence) != nil || sequence < first || sequence > last {
					return attachment.ErrInvalid
				}
			}
			fields := make(map[string]string, len(row))
			for i, value := range row {
				var scalar any
				if json.Unmarshal(value, &scalar) != nil {
					return attachment.ErrInvalid
				}
				switch v := scalar.(type) {
				case nil, float64:
				case string:
					fields[table.Columns[i]] = v
				default:
					return attachment.ErrInvalid
				}
			}
			for column, expected := range map[string]string{"organization_id": section.OrganizationID, "project_id": section.ProjectID, "work_item_id": section.WorkItemID} {
				if slices.Contains(table.Columns, column) && fields[column] != expected {
					return attachment.ErrInvalid
				}
			}
			var column string
			switch table.Name {
			case "native_attempts":
				column = "id"
			case "native_attempt_events", "attempt_diffs":
				column = "attempt_id"
			case "change_versions", "change_evidence":
				column = "change_id"
			case "collaboration_versions":
				column = "record_id"
			case "conversation_messages", "conversation_events":
				column = "conversation_id"
			case "collaboration_events":
				column = "work_item_id"
			case "attempt_diff_files":
				continue
			}
			if fields[column] != subject {
				return attachment.ErrInvalid
			}
			if table.Name == "attempt_diffs" {
				parents[fields["id"]] = true
			}
		}
	}
	if records == 0 {
		return attachment.ErrInvalid
	}
	for _, table := range section.Tables {
		if table.Name != "attempt_diff_files" {
			continue
		}
		index := slices.Index(table.Columns, "diff_id")
		for _, row := range table.Rows {
			var id string
			if index < 0 || json.Unmarshal(row[index], &id) != nil || !parents[id] {
				return attachment.ErrInvalid
			}
		}
	}
	return nil
}
