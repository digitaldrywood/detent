package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *conversationService) compactEventsPG(ctx context.Context, organization tracker.OrganizationID, now time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	before := now.Add(-conversationstore.EventRetention)
	var candidates conversationstore.CompactionPage
	if err := c.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		owner, ok := reader.(conversationstore.CompactionReader)
		if !ok {
			return conversationUnsupported("Conversation compaction owner is unavailable")
		}
		var err error
		candidates, err = owner.CompactionCandidates(ctx, c.lifecycleCursor("compaction"), before, now, conversationstore.CompactionConversations)
		return err
	}); err != nil {
		return 0, err
	}
	remaining, removed := conversationstore.CompactionBatch, 0
	processedCandidates := 0
	for _, candidate := range candidates.Items {
		if remaining == 0 {
			break
		}
		processed, count, err := c.compactConversationEventsPG(ctx, candidate.Conversation, before, now, remaining)
		c.advanceLifecycleCursor("compaction", candidate.Cursor)
		processedCandidates++
		if err != nil {
			return removed, fmt.Errorf("compact PostgreSQL conversation events: %w", err)
		}
		remaining -= processed
		removed += count
	}
	if processedCandidates == len(candidates.Items) && candidates.Next == "" {
		c.advanceLifecycleCursor("compaction", "")
	}
	return removed, nil
}

func (c *conversationService) compactConversationEventsPG(ctx context.Context, candidate conversationstore.Conversation, before, now time.Time, budget int) (int, int, error) {
	organization := candidate.OrganizationID
	var metadata conversationstore.EventPage
	var full bool
	if err := c.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		owner, ok := reader.(conversationstore.CompactionReader)
		if !ok {
			return conversationUnsupported("Conversation compaction owner is unavailable")
		}
		var err error
		metadata, err = owner.UnprojectedConversationEvents(ctx, candidate.ID, budget)
		return err
	}); err != nil {
		return 0, 0, err
	}
	processed := len(metadata.Items)
	if processed > 0 {
		if err := c.server.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(writer tenantstore.Writer) error {
			owner, ok := writer.(conversationstore.CompactionWriter)
			if !ok {
				return conversationUnsupported("Conversation compaction owner is unavailable")
			}
			return owner.ProjectConversationEvents(ctx, candidate.ID, metadata.Items)
		}); err != nil {
			return processed, 0, err
		}
	}
	if processed == budget || metadata.Next != 0 {
		return processed, 0, nil
	}
	var events conversationstore.CompactionEventPage
	var reference attachment.ArchiveReference
	if err := c.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		owner, ok := reader.(conversationstore.CompactionReader)
		if !ok {
			return conversationUnsupported("Conversation compaction owner is unavailable")
		}
		var err error
		candidate, err = reader.Conversation(ctx, candidate.ProjectID, candidate.ID)
		if err != nil {
			return err
		}
		full, err = owner.ConversationFullCompactionEligible(ctx, candidate.ProjectID, candidate.ID, before, now)
		if err != nil {
			return err
		}
		after := candidate.Compaction.ContentScannedSequence
		if full {
			after = candidate.Compaction.ScannedSequence
		}
		events, err = owner.ObsoleteConversationEvents(ctx, candidate.ID, after, candidate.EventSequence, full, budget-processed)
		if err != nil || len(events.Items) == 0 || candidate.Compaction.ContentReference == "" {
			return err
		}
		object, err := reader.ConversationContent(ctx, candidate.Compaction.ContentReference)
		reference = object.Reference
		return err
	}); err != nil {
		return processed, 0, err
	}
	processed += events.ScannedCount
	state := candidate.Compaction
	scanThrough := events.ScannedSequence
	if events.Complete {
		scanThrough = candidate.EventSequence
	}
	state.ContentScannedSequence = max(state.ContentScannedSequence, scanThrough)
	if full {
		state.ScannedSequence = max(state.ScannedSequence, scanThrough)
	}
	plan := &conversationPublicationPlan{service: c, record: conversationRecordFromStore(candidate), now: now}
	var sequences []int64
	if len(events.Items) > 0 {
		var summary tracker.EventCompaction
		if state.ContentReference != "" {
			var payload conversationContent
			if err := c.server.readConversationObject(ctx, string(organization), reference, &payload); err != nil {
				return processed, 0, err
			}
			if err := json.Unmarshal(payload.Data, &summary); err != nil {
				return processed, 0, fmt.Errorf("decode conversation compaction summary: %w", err)
			}
			if summary.ThroughSequence != state.ThroughSequence {
				return processed, 0, conversationContentUnavailable()
			}
		}
		if summary.RemovedByType == nil {
			summary.RemovedByType = make(map[string]int64)
		}
		for _, event := range events.Items {
			sequences = append(sequences, event.Seq)
			summary.RemovedEvents++
			summary.RemovedBytes += int64(len(event.Data))
			summary.RemovedByType[string(event.Type)]++
			summary.ThroughSequence = max(summary.ThroughSequence, event.Seq)
		}
		summary.RetainedEvents = candidate.EventSequence - summary.RemovedEvents
		summary.CompactedAt = now.UTC()
		raw, err := json.Marshal(summary)
		if err != nil {
			return processed, 0, err
		}
		content, err := json.Marshal(conversationContent{Data: raw})
		if err != nil {
			return processed, 0, err
		}
		ref, err := plan.prepare(ctx, content)
		if err != nil {
			return processed, 0, err
		}
		state.ContentReference = contentArchiveID(string(organization), ref)
		state.ThroughSequence = summary.ThroughSequence
	}
	changed := false
	err := c.server.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(writer tenantstore.Writer) error {
		owner, ok := writer.(conversationstore.CompactionWriter)
		if !ok {
			return conversationUnsupported("Conversation compaction owner is unavailable")
		}
		var err error
		changed, err = owner.CompactConversationEvents(ctx, candidate.ProjectID, conversationstore.CompactionRequest{Conversation: candidate, Sequences: sequences, State: state, Before: before, Now: now, Full: full})
		if err != nil || !changed {
			return err
		}
		return registerPreparedConversationObjects(ctx, writer, organization, plan.prepared, now)
	})
	if err != nil || !changed {
		return processed, 0, err
	}
	if len(sequences) > 0 {
		c.broker.notify(candidate.ID)
	}
	return processed, len(sequences), nil
}
