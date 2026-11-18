package conversationstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
)

type EventMetadata struct {
	MessageID       *string         `json:"message_id"`
	ObjectID        *string         `json:"id"`
	Role            *string         `json:"role"`
	Kind            *string         `json:"kind"`
	AttemptID       *string         `json:"attempt_id"`
	Status          *string         `json:"status"`
	ThreadID        *string         `json:"thread_id"`
	TurnID          *string         `json:"turn_id"`
	Title           *string         `json:"title"`
	Visibility      *string         `json:"visibility"`
	SubjectWorkItem *string         `json:"subject_work_item_id"`
	WorkItem        *string         `json:"work_item_id"`
	LinkedAt        *string         `json:"linked_at"`
	DeltaOffset     *int64          `json:"seq"`
	DeltaBytes      *int64          `json:"text_bytes"`
	MessageRevision *int64          `json:"message_revision"`
	Preferences     json.RawMessage `json:"preferences"`
	PreferencesKey  *string         `json:"-"`
	Model           *string         `json:"-"`
	Effort          *string         `json:"-"`
	Access          *string         `json:"-"`
}

func (m EventMetadata) CompactionGroup(kind conversation.EventType) (string, error) {
	var fields []any
	switch kind {
	case conversation.EventMessageUpdated:
		if m.ObjectID == nil {
			return "", nil
		}
		fields = []any{m.ObjectID}
	case conversation.EventExecutionUpdated:
		fields = []any{m.AttemptID, m.Status, m.ThreadID, m.TurnID}
	case conversation.EventConversationUpdated:
		var linked *time.Time
		if m.LinkedAt != nil {
			at, err := time.Parse(time.RFC3339Nano, *m.LinkedAt)
			if err != nil {
				return "", err
			}
			linked = new(at.UTC())
		}
		fields = []any{m.Title, m.Visibility, m.Status, m.PreferencesKey, m.SubjectWorkItem, m.WorkItem, linked}
	default:
		return "", nil
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func ProjectEventMetadata(raw []byte) (EventMetadata, error) {
	var metadata EventMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return metadata, err
	}
	if len(metadata.Preferences) == 0 || string(metadata.Preferences) == "null" {
		return metadata, nil
	}
	var preferences struct {
		Model  *string `json:"model"`
		Effort *string `json:"reasoning_effort"`
		Access *string `json:"access"`
	}
	if err := json.Unmarshal(metadata.Preferences, &preferences); err != nil {
		return metadata, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, metadata.Preferences); err != nil {
		return metadata, err
	}
	digest := sha256.Sum256(compact.Bytes())
	metadata.PreferencesKey = new(hex.EncodeToString(digest[:]))
	metadata.Model, metadata.Effort, metadata.Access = preferences.Model, preferences.Effort, preferences.Access
	return metadata, nil
}
