package workitemread

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const ListPageReserveBytes = 8192

func (rd Reader) ListBodies(ctx context.Context, scope hubscope.NativeScope, items []tracker.NativeIssue, budget int) (int, error) {
	keys := make([]eventstore.CollaborationVersionKey, 0, len(items))
	for _, item := range items {
		if slices.Contains(item.OmittedFields, "body") {
			keys = append(keys, eventstore.CollaborationVersionKey{RecordID: string(item.WorkItemID), Revision: int64(item.Revision)})
		}
	}
	versions := make(map[eventstore.CollaborationVersionKey]eventstore.CollaborationVersion, len(keys))
	if len(keys) > 0 {
		if err := rd.deps.Tenants.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := rd.deps.Authorize(ctx, r, scope, false); err != nil {
				return err
			}
			batch, ok := r.(eventstore.CollaborationVersionsReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			found, err := batch.CollaborationVersions(ctx, keys)
			for _, version := range found {
				versions[eventstore.CollaborationVersionKey{RecordID: version.RecordID, Revision: version.Revision}] = version
			}
			return err
		}); err != nil {
			return 0, hubapi.ProjectRecordError(err)
		}
	}
	used := ListPageReserveBytes
	for index := range items {
		item := &items[index]
		raw, err := json.Marshal(item)
		if err != nil {
			return 0, err
		}
		size := len(raw)
		if slices.Contains(item.OmittedFields, "body") {
			version, found := versions[eventstore.CollaborationVersionKey{RecordID: string(item.WorkItemID), Revision: int64(item.Revision)}]
			if !found || version.WorkItemID != item.WorkItemID || version.ProjectID != scope.Project {
				return 0, artifact.ErrIntegrity
			}
			if used+size+int(version.PayloadContentBytes) <= budget {
				payload, err := eventstore.NewContent(rd.deps.Details).CollaborationVersion(ctx, version)
				if err != nil {
					return 0, err
				}
				full, err := eventstore.DecodeCollaborationIssue(version, payload)
				if err != nil {
					return 0, err
				}
				complete := *item
				complete.Body = full.Body
				complete.OmittedFields = slices.DeleteFunc(slices.Clone(item.OmittedFields), func(field string) bool { return field == "body" })
				if raw, err = json.Marshal(complete); err != nil {
					return 0, err
				}
				if used+len(raw) <= budget {
					*item, size = complete, len(raw)
				}
			}
			if slices.Contains(item.OmittedFields, "body") {
				if index > 0 {
					return index, nil
				}
				return 1, nil
			}
		}
		if used+size > budget && index > 0 {
			return index, nil
		}
		used += size
	}
	return len(items), nil
}
