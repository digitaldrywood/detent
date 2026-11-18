package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const issueStorageArchiveAge = 72 * time.Hour
const issueStorageArchiveBatch = 500

func (s *Service) storageArchiveContent() entryauthority.ContentHandler {
	if s.config.Content != nil {
		return s.config.Content
	}
	if s.config.Hosted != nil && s.config.Hosted.SharedEntry != nil && s.config.Hosted.SharedEntry.Content != nil {
		return s.config.Hosted.SharedEntry.Content
	}
	return nil
}

func (s *Service) archiveClosedNativeIssues(ctx context.Context, now time.Time) error {
	if s.config.TenantRepository == nil || s.storageArchiveContent() == nil {
		return nil
	}
	organization := tracker.OrganizationID(s.appOrganizationID())
	if organization == "" {
		return errors.New("issue storage archives require an organization")
	}
	return s.archiveClosedNativeIssuesTenant(ctx, organization, now)
}

func (s *Service) archiveClosedNativeIssuesTenant(ctx context.Context, organization tracker.OrganizationID, now time.Time) error {
	if organization == "" {
		return errors.New("issue storage archives require an organization")
	}
	before := now.Add(-issueStorageArchiveAge)
	var after attachmentstore.StorageArchiveCursor
	var candidates attachmentstore.StorageArchivePage
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		owner, ok := reader.(attachmentstore.StorageArchiveReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		after, err = owner.StorageArchiveScanCursor(ctx)
		if err != nil {
			return err
		}
		candidates, err = owner.StorageArchiveCandidates(ctx, before, after, 4)
		return err
	})
	if err != nil {
		return err
	}
	var failures error
	advance := func(next attachmentstore.StorageArchiveCursor) error {
		checkpoint, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubapi.ReadTimeout)
		defer cancel()
		err := s.config.TenantRepository.WriteTenant(checkpoint, organization, corestore.Background, func(w tenantstore.Writer) error {
			owner, ok := w.(attachmentstore.StorageArchiveWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			changed, err := owner.AdvanceStorageArchiveScan(checkpoint, after, next)
			if err == nil && !changed {
				return corestore.ErrRevisionConflict
			}
			return err
		})
		if err == nil {
			after = next
		}
		return err
	}
	attempted := 0
	for _, candidate := range candidates.Items {
		if err := ctx.Err(); err != nil {
			failures = errors.Join(failures, err)
			break
		}
		err := s.publishStorageArchive(ctx, organization, candidate.Stamp, now)
		failures = errors.Join(failures, err)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return failures
		}
		if err := advance(candidate.Cursor); err != nil {
			return errors.Join(failures, err)
		}
		attempted++
	}
	if attempted == len(candidates.Items) && candidates.Next.ID == 0 {
		if err := advance(attachmentstore.StorageArchiveCursor{}); err != nil {
			return errors.Join(failures, err)
		}
	}
	return errors.Join(failures, s.retireStorageArchiveSections(ctx, organization, now))
}

func (s *Service) uploadStorageArchiveDocument(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, uploader string, document attachmentstore.StorageArchiveDocument, now time.Time) (attachmentstore.ArchiveRecord, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return attachmentstore.ArchiveRecord{}, fmt.Errorf("encode issue archive: %w", err)
	}
	if len(raw) > attachment.MaxSectionBytes {
		return attachmentstore.ArchiveRecord{}, attachment.ErrTooLarge
	}
	ref, data, err := attachment.PrepareArchive(string(organization), 2, bytes.NewReader(raw))
	if err != nil {
		return attachmentstore.ArchiveRecord{}, err
	}
	if _, err := s.storageArchiveContent()(ctx, entryauthority.ContentRequest{Organization: string(organization), Reference: ref, Data: data, Write: true}); err != nil {
		return attachmentstore.ArchiveRecord{}, err
	}
	return attachmentstore.ArchiveRecord{OrganizationID: organization, ProjectID: project, ID: "issue_archive_" + ref.SHA256, Uploader: uploader, CreatedAt: now, Reference: ref}, nil
}

func (s *Service) publishStorageArchive(ctx context.Context, organization tracker.OrganizationID, stamp attachmentstore.StorageArchiveStamp, now time.Time) error {
	var snapshot attachmentstore.StorageArchiveSnapshot
	var uploader string
	var uploaderErr error
	before := now.Add(-issueStorageArchiveAge)
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		owner, ok := reader.(attachmentstore.StorageArchiveReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		snapshot, err = owner.StorageArchiveSnapshot(ctx, stamp, before, issueStorageArchiveBatch)
		if err != nil {
			return err
		}
		uploader, uploaderErr = owner.StorageArchiveUploader(ctx, stamp.ProjectID)
		return nil
	})
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, corestore.ErrRevisionConflict) {
			return nil
		}
		return err
	}
	publication := attachmentstore.StorageArchivePublication{Stamp: stamp, PublishedAt: now, ThroughHistory: snapshot.ThroughHistory, LastMessageScanned: snapshot.LastMessageScanned}
	commit := func() error {
		return s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(writer tenantstore.Writer) error {
			owner, ok := writer.(attachmentstore.StorageArchiveWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			_, err := owner.PublishStorageArchive(ctx, publication, before)
			return err
		})
	}
	if snapshot.Previous != nil {
		publication.ExpectedRootID = snapshot.Previous.Root.ID
		publication.ExpectedMessageCursor = snapshot.Previous.LastMessageScanned
	}
	if snapshot.Previous != nil && snapshot.Previous.Stamp == stamp && snapshot.Previous.ThroughHistory >= stamp.EventSequence && len(snapshot.Messages) == 0 {
		if snapshot.Previous.LastMessageScanned == snapshot.LastMessageScanned {
			return nil
		}
		publication.Root = snapshot.Previous.Root
		return commit()
	}
	if uploaderErr != nil {
		return fmt.Errorf("resolve issue archive uploader: %w", uploaderErr)
	}
	root := attachmentstore.StorageArchiveDocument{SchemaVersion: 2, OrganizationID: organization, ProjectID: stamp.ProjectID, WorkItemID: stamp.WorkItemID, Kind: "root", Attempt: snapshot.Attempt, Item: new(snapshot.Item)}
	if snapshot.Previous != nil {
		publication.ExpectedRootID = snapshot.Previous.Root.ID
		root.Previous = new(snapshot.Previous.Root.Reference)
	}
	for _, record := range snapshot.History {
		root.History = append(root.History, eventstore.Event{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, ID: record.ID, Sequence: record.Sequence, Type: record.Type, SchemaVersion: record.SchemaVersion, ActorKind: record.ActorKind, ActorPrincipalID: record.ActorPrincipalID, ActorEnvelopeKnown: record.ActorEnvelopeKnown, ActorViaKey: record.ActorViaKey, ActorDisplayName: record.ActorDisplayName, ActorAgent: record.ActorAgent, EventRevision: record.EventRevision, RecordedAt: record.RecordedAt, RunID: record.RunID, AttemptID: record.AttemptID, FromState: record.FromState, ToState: record.ToState, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, PayloadFormatVersion: record.PayloadFormatVersion})
		publication.ThroughHistory = max(publication.ThroughHistory, record.Sequence)
		if !strings.HasPrefix(record.Type, "run.") {
			continue
		}
		event, _, err := s.readTenantCollaborationEvent(s.runDetailContext(ctx), record)
		if err != nil {
			return err
		}
		data := tracker.CollaborationData{Revision: event.Data.Revision}
		if run := event.Data.Run; run != nil {
			data.Run = &tracker.NativeRunData{AttemptID: run.AttemptID, RunID: run.RunID, Sequence: run.Sequence, Outcome: run.Outcome}
		}
		event.Data = data
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		root.Timeline = append(root.Timeline, raw)
	}
	root.Last = publication.ThroughHistory
	contents := make(map[string]conversationstore.ContentObject, len(snapshot.Contents))
	for _, content := range snapshot.Contents {
		contents[content.ID] = content
	}
	for index := range snapshot.Messages {
		message := &snapshot.Messages[index]
		publication.MessageSources = append(publication.MessageSources, attachmentstore.StorageArchiveMessageSource{ConversationID: message.ConversationID, MessageID: message.ID, Sequence: message.Sequence, Revision: message.Revision, ContentReference: message.ContentReference})
		if message.ContentReference != "" {
			message.Text = ""
			message.Data = json.RawMessage(`{}`)
			continue
		}
		if s.conversations == nil || s.conversations.Store == nil {
			return conversationContentUnavailable()
		}
		raw, err := messageContent(conversationMessageFromStore(*message))
		if err != nil {
			return err
		}
		ref, err := s.conversations.Store.PublishPayload(ctx, string(organization), raw)
		if err != nil {
			return err
		}
		content := conversationstore.ContentObject{OrganizationID: organization, ID: contentArchiveID(string(organization), ref), Reference: ref, CreatedAt: now}
		publication.MessagePayloads = append(publication.MessagePayloads, attachmentstore.StorageArchiveMessagePayload{ConversationID: message.ConversationID, MessageID: message.ID, Sequence: message.Sequence, Revision: message.Revision, ExpectedReference: message.ContentReference, Content: content})
		contents[content.ID] = content
		message.ContentReference = content.ID
		message.Text = ""
		message.Data = json.RawMessage(`{}`)
	}
	messages := snapshot.Messages
	for len(messages) > 0 && len(publication.Sections) < 4 {
		end, size := 0, 0
		for end < len(messages) {
			raw, err := json.Marshal(messages[end])
			if err != nil {
				return err
			}
			content, ok := contents[messages[end].ContentReference]
			if !ok {
				return conversationContentUnavailable()
			}
			reference, err := json.Marshal(attachmentstore.StorageArchiveContentReference{ID: content.ID, Reference: content.Reference})
			if err != nil {
				return err
			}
			cost := len(raw) + len(reference) + 2
			if size+cost > attachment.MaxSectionBytes-(64<<10) {
				break
			}
			size += cost
			end++
		}
		if end == 0 {
			return attachment.ErrTooLarge
		}
		chunk := messages[:end]
		first, last := chunk[0].Sequence, chunk[len(chunk)-1].Sequence
		subject := chunk[0].ConversationID
		chunkData, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		id := "conversation_messages:" + subject + ":" + artifact.Digest(chunkData)
		document := attachmentstore.StorageArchiveDocument{SchemaVersion: 2, OrganizationID: organization, ProjectID: stamp.ProjectID, WorkItemID: stamp.WorkItemID, SectionID: id, Kind: "conversation_messages", Subject: subject, First: first, Last: last, Messages: chunk}
		included := map[string]bool{}
		for _, message := range chunk {
			if included[message.ContentReference] {
				continue
			}
			content, ok := contents[message.ContentReference]
			if !ok {
				return conversationContentUnavailable()
			}
			included[message.ContentReference] = true
			document.Contents = append(document.Contents, attachmentstore.StorageArchiveContentReference{ID: content.ID, Reference: content.Reference})
		}
		record, err := s.uploadStorageArchiveDocument(ctx, organization, stamp.ProjectID, uploader, document, now)
		if err != nil {
			return err
		}
		publication.Sections = append(publication.Sections, attachmentstore.StorageArchiveSection{OrganizationID: organization, Stamp: stamp, Kind: document.Kind, Subject: subject, First: first, Last: last, ID: id, Archive: record})
		messages = messages[end:]
	}
	for _, section := range publication.Sections {
		root.Sections = append(root.Sections, attachmentstore.StorageArchiveManifestSection{Kind: section.Kind, Subject: section.Subject, First: section.First, Last: section.Last, ID: section.ID, Reference: section.Archive.Reference})
	}
	rootRecord, err := s.uploadStorageArchiveDocument(ctx, organization, stamp.ProjectID, uploader, root, now)
	if err != nil {
		return err
	}
	publication.Root = rootRecord
	return commit()
}

func (s *Service) retireStorageArchiveSections(ctx context.Context, organization tracker.OrganizationID, now time.Time) error {
	var after attachmentstore.StorageArchiveRetirementCursor
	var page attachmentstore.StorageArchiveRetirementPage
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		owner, ok := reader.(attachmentstore.StorageArchiveReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		after, err = owner.StorageArchiveRetirementCursor(ctx)
		if err != nil {
			return err
		}
		page, err = owner.StorageArchiveRetirements(ctx, after, 4)
		return err
	})
	if err != nil {
		return err
	}
	advance := func(next attachmentstore.StorageArchiveRetirementCursor) error {
		return s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
			owner, ok := w.(attachmentstore.StorageArchiveWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			changed, err := owner.AdvanceStorageArchiveRetirement(ctx, after, next)
			if err == nil && !changed {
				return corestore.ErrRevisionConflict
			}
			if err == nil {
				after = next
			}
			return err
		})
	}
	remaining, processedSections := issueStorageArchiveBatch, 0
	for _, section := range page.Items {
		if remaining == 0 {
			break
		}
		result := attachmentstore.StorageArchiveRetirementResult{}
		err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(writer tenantstore.Writer) error {
			owner, ok := writer.(attachmentstore.StorageArchiveWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			var err error
			result, err = owner.RetireStorageArchiveSection(ctx, section, now.Add(-issueStorageArchiveAge), remaining)
			return err
		})
		if err != nil && !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, corestore.ErrRevisionConflict) {
			return err
		}
		if result.Complete || err != nil {
			if err := advance(attachmentstore.StorageArchiveRetirementCursor{ProjectID: section.Stamp.ProjectID, WorkItemID: section.Stamp.WorkItemID, Revision: section.Stamp.Revision, SectionID: section.ID}); err != nil {
				return err
			}
			processedSections++
		}
		remaining -= result.Processed
	}
	if processedSections == len(page.Items) && page.Next.SectionID == "" {
		return advance(attachmentstore.StorageArchiveRetirementCursor{})
	}
	return nil
}
