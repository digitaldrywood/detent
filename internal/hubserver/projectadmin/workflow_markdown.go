package projectadmin

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
)

func WorkflowMarkdown(ctx context.Context, details attachment.DetailStore, v projectstore.Settings) (string, error) {
	if v.WorkflowKey == "" {
		return "", nil
	}
	raw, err := eventstore.NewContent(details).Receipt(ctx, eventstore.Receipt{OrganizationID: v.OrganizationID, PayloadObjectKey: v.WorkflowKey, PayloadSHA256: v.WorkflowSHA256, PayloadBytes: v.WorkflowBytes, PayloadContentBytes: v.WorkflowContentBytes})
	if err != nil {
		return "", err
	}
	var markdown string
	err = json.Unmarshal(raw, &markdown)
	return markdown, err
}
