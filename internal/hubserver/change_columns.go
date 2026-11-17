package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func decodeChangeColumnsJSON(ctx context.Context, raw []byte, result any) error {
	if bytes.HasPrefix(raw, []byte("orgs/")) {
		return hubcontent.DecodeStorageJSON(ctx, raw, result)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	for key := range object {
		if key != "_storage_archive" {
			return json.Unmarshal(raw, result)
		}
	}
	reference, found := object["_storage_archive"]
	if !found {
		return json.Unmarshal(raw, result)
	}
	var ref storageJSONReference
	if err := json.Unmarshal(reference, &ref); err != nil {
		return err
	}
	data, err := hubcontent.ReadChangeContent(ctx, ref.Organization, ref.Reference)
	if err != nil {
		return err
	}
	var payload storageJSONPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	if payload.Organization != ref.Organization || payload.Project != ref.Project || payload.Family != ref.Family || payload.Row != ref.Row || !json.Valid(payload.Data) {
		return artifact.ErrIntegrity
	}
	return json.Unmarshal(payload.Data, result)
}

type changeColumnCursor struct {
	Review         string
	Checkpoint     string
	CheckpointTurn bool
}

func (s *Service) populateChangeColumns(ctx context.Context) error {
	var cursor changeColumnCursor
	switch prior := s.changeColumnAfter.(type) {
	case changeColumnCursor:
		cursor = prior
	case string:
		cursor.Review = prior
	}
	checkpointTurn := cursor.CheckpointTurn
	cursor.CheckpointTurn = !cursor.CheckpointTurn
	defer func() { s.changeColumnAfter = cursor }()
	if checkpointTurn {
		next, err := s.populateCheckpointSource(ctx, tracker.OrganizationID(s.appOrganizationID()), cursor.Checkpoint)
		if next != "" || err == nil {
			cursor.Checkpoint = next
		}
		return err
	}
	owner, ok := s.config.TenantRepository.(interface {
		BackfillCurrentChangeReviewsAfter(context.Context, tracker.OrganizationID, *eventstore.Content, int, string) (int, string, error)
	})
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	_, next, err := owner.BackfillCurrentChangeReviewsAfter(ctx, tracker.OrganizationID(s.appOrganizationID()), eventstore.NewContent(s.runDetailStore()), corestore.MaxPageSize, cursor.Review)
	if next != "" || err == nil {
		cursor.Review = next
	}
	return err
}

func changeColumnTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
