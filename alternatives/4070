package threadstore

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Content struct{ owner *eventstore.Content }

func NewContent(store attachment.DetailStore) *Content {
	return &Content{owner: eventstore.NewContent(store)}
}
func (c *Content) PreparePost(ctx context.Context, organization tracker.OrganizationID, post PostRecord) (PostRecord, error) {
	raw, err := json.Marshal(post.Body)
	if err != nil {
		return PostRecord{}, err
	}
	receipt, err := c.owner.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: organization}, raw)
	if err != nil {
		return PostRecord{}, err
	}
	post.BodyReference = eventstore.Reference(receipt.PayloadObjectKey, receipt.PayloadSHA256, receipt.PayloadBytes, receipt.PayloadContentBytes)
	return post, nil
}
func (c *Content) ReadPost(ctx context.Context, organization tracker.OrganizationID, post PostRecord) (PostRecord, error) {
	ref := post.BodyReference
	raw, err := c.owner.Receipt(ctx, eventstore.Receipt{OrganizationID: organization, PayloadObjectKey: ref.Key, PayloadSHA256: ref.SHA256, PayloadBytes: ref.CompressedSize, PayloadContentBytes: ref.UncompressedSize})
	if err != nil {
		return PostRecord{}, err
	}
	if err := json.Unmarshal(raw, &post.Body); err != nil {
		return PostRecord{}, err
	}
	return post, nil
}
