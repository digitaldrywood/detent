package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readHistoryTenant(ctx context.Context, scope nativeScope, item string, params url.Values) (tracker.Page[tracker.CollaborationEvent], error) {
	ctx = s.runDetailContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if params.Get("archive_url") == "true" {
		return tracker.Page[tracker.CollaborationEvent]{}, nativePublicInvalid("archive_url is not supported for PostgreSQL-backed history")
	}
	if err := validateNativeQuery(params, "view", "direction"); err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	view := params.Get("view")
	if view != "" && view != "blockers" && view != "timeline" {
		return tracker.Page[tracker.CollaborationEvent]{}, nativePublicInvalid("view supports blockers or timeline")
	}
	newest := params.Get("direction") == "newest"
	if newest && view == "" {
		return tracker.Page[tracker.CollaborationEvent]{}, nativePublicInvalid("Newest history requires the timeline or blockers view")
	}
	path := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(item) + "/history"
	limit, cursor, key, err := s.readNativePage(ctx, scope, path, params)
	if err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	boundary := int64(0)
	if newest {
		boundary = 1<<63 - 1
	}
	if cursor.After != "" {
		boundary, err = strconv.ParseInt(cursor.After, 10, 64)
		if err != nil || boundary < 0 {
			return tracker.Page[tracker.CollaborationEvent]{}, nativePublicInvalid("History cursor is invalid")
		}
	}

	var records []eventstore.Event
	var hasMore bool
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		workItem, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(workItem.ActorPrincipalID)}) {
			return nativeNotFound()
		}
		if newest {
			records, err = r.EventsBefore(ctx, scope.project, workItem.NativeID, boundary, limit)
		} else {
			records, err = r.Events(ctx, scope.project, workItem.NativeID, boundary, limit)
		}
		if err != nil || len(records) < limit {
			return err
		}
		last := records[len(records)-1].Sequence
		var lookahead []eventstore.Event
		if newest {
			lookahead, err = r.EventsBefore(ctx, scope.project, workItem.NativeID, last, 1)
		} else {
			lookahead, err = r.Events(ctx, scope.project, workItem.NativeID, last, 1)
		}
		hasMore = len(lookahead) != 0
		return err
	})
	if err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	events := make([]tracker.CollaborationEvent, 0, len(records))
	payloads := make([][]byte, 0, len(records))
	for _, record := range records {
		event, raw, err := s.readTenantCollaborationEvent(ctx, record)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		switch view {
		case "timeline":
			if strings.HasPrefix(event.Type, "run.") {
				event.Data = tracker.CollaborationData{}
			}
		case "blockers":
			data := event.Data
			event.Data = tracker.CollaborationData{Revision: data.Revision, FromState: data.FromState, ToState: data.ToState, Reason: data.Reason}
			if data.Run != nil {
				event.Data.Run = &tracker.NativeRunData{AttemptID: data.Run.AttemptID, FencingToken: data.Run.FencingToken}
			}
		default:
			if event.Data.Run != nil {
				event.Data.Run.Runtime = event.Data.Run.Runtime.WithoutActivitySpans()
			}
		}
		events = append(events, event)
		payloads = append(payloads, raw)
	}
	if err := s.hydrateTenantHistoryRelatedIssues(ctx, scope, events); err != nil {
		return tracker.Page[tracker.CollaborationEvent]{}, err
	}
	page := tracker.Page[tracker.CollaborationEvent]{Items: []tracker.CollaborationEvent{}}
	size := 8192
	for index, event := range events {
		record := records[index]
		raw := payloads[index]
		encoded, err := json.Marshal(event)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
		if len(encoded) > operatortool.WorkHistoryPageBytes-8192 {
			digest := sha256.Sum256(raw)
			event.Data = tracker.CollaborationData{}
			event.DataOmission = &tracker.HistoryDataOmission{Bytes: len(raw), SHA256: hex.EncodeToString(digest[:])}
			encoded, err = json.Marshal(event)
			if err != nil {
				return tracker.Page[tracker.CollaborationEvent]{}, err
			}
		}
		if size+len(encoded)+1 > operatortool.WorkHistoryPageBytes {
			if len(page.Items) == 0 {
				return tracker.Page[tracker.CollaborationEvent]{}, operatortool.ErrReadUnavailable
			}
			hasMore = true
			break
		}
		size += len(encoded) + 1
		page.Items = append(page.Items, event)
		cursor.After = strconv.FormatInt(record.Sequence, 10)
	}
	if hasMore {
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return tracker.Page[tracker.CollaborationEvent]{}, err
		}
	}
	return page, nil
}

func (s *Service) hydrateTenantHistoryRelatedIssues(ctx context.Context, scope nativeScope, events []tracker.CollaborationEvent) error {
	indices := make(map[tracker.NativeWorkItemID][]int)
	for index, event := range events {
		if event.Data.RelatedWorkItemID != "" {
			indices[event.Data.RelatedWorkItemID] = append(indices[event.Data.RelatedWorkItemID], index)
		}
	}
	if len(indices) == 0 {
		return nil
	}
	return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		organization, err := r.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		for id, positions := range indices {
			item, err := r.WorkItemByNativeID(ctx, id)
			if errors.Is(err, corestore.ErrNotFound) {
				clearTenantHistoryRelation(events, positions)
				continue
			}
			if err != nil {
				return err
			}
			if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
				clearTenantHistoryRelation(events, positions)
				continue
			}
			targetScope := scope
			targetScope.project = item.ProjectID
			if err := s.authorizeAccessScope(ctx, r, targetScope, false); err != nil {
				if errors.Is(err, auth.ErrHostedIdentity) || errors.Is(err, accessstore.ErrDenied) {
					clearTenantHistoryRelation(events, positions)
					continue
				}
				return err
			}
			record, err := r.ProjectRecord(ctx, item.ProjectID)
			if err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					clearTenantHistoryRelation(events, positions)
					continue
				}
				return err
			}
			state := ""
			if item.WorkflowStateName != nil {
				state = *item.WorkflowStateName
			}
			terminal := false
			if workflowState := slices.IndexFunc(record.States, func(candidate tracker.NativeState) bool { return candidate.Name == state }); workflowState >= 0 {
				terminal = record.States[workflowState].Terminal
			}
			link := &tracker.NativeDependency{ID: item.NativeID, ProjectID: item.ProjectID, Number: int(item.Number), Title: item.Title, State: state, Terminal: terminal, WebURL: issueBrowserPath(organization.Slug, record.Core.Slug, int(item.Number))}
			for _, position := range positions {
				events[position].Data.RelatedIssue = link
			}
		}
		return nil
	})
}

func clearTenantHistoryRelation(events []tracker.CollaborationEvent, positions []int) {
	for _, position := range positions {
		events[position].Data.RelatedIssue = nil
		events[position].Data.RelatedWorkItemID = ""
	}
}

func (s *Service) readTenantCollaborationEvent(ctx context.Context, record eventstore.Event) (tracker.CollaborationEvent, []byte, error) {
	store := s.runDetailStore()
	if store == nil {
		return tracker.CollaborationEvent{}, nil, nativeRunEventStorageUnavailable()
	}
	ref := eventstore.Reference(record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes)
	raw, err := store.ReadDetail(ctx, string(record.OrganizationID), ref)
	if err != nil {
		return tracker.CollaborationEvent{}, nil, err
	}
	event, err := eventstore.NewContent(store).Event(ctx, record)
	if err != nil {
		return tracker.CollaborationEvent{}, nil, err
	}
	if event.Data.Run != nil {
		var envelope struct {
			Data struct {
				Run json.RawMessage `json:"run"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return tracker.CollaborationEvent{}, nil, err
		}
		if len(envelope.Data.Run) > 0 {
			if err := decodeRunDetail(ctx, nil, string(envelope.Data.Run), event.Data.Run); err != nil {
				return tracker.CollaborationEvent{}, nil, err
			}
		}
	}
	return event, raw, nil
}
