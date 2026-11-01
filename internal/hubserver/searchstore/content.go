package searchstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Content struct{ store attachment.DetailStore }
type payload struct {
	Document Document
	Text     Text
}

func NewContent(store attachment.DetailStore) *Content { return &Content{store: store} }

func (c *Content) Refresher(repository Repository, organization tracker.OrganizationID) Refresh {
	return func(ctx context.Context) (bool, error) { return c.RefreshOne(ctx, repository, organization) }
}

func (c *Content) Prepare(ctx context.Context, document Document, text Text) (Document, error) {
	if c.store == nil {
		return Document{}, artifact.ErrStorage
	}
	if err := document.Validate(); err != nil {
		return Document{}, err
	}
	document.Generation, document.Placeholder = 0, false
	text = boundedText(text)
	document.Title = strings.ReplaceAll(text.Title, "\x00", "")
	document.UpdatedAt = document.UpdatedAt.UTC()
	document.Payload = attachment.ArchiveReference{}
	raw, err := json.Marshal(payload{document, text})
	if err != nil {
		return Document{}, fmt.Errorf("encode search content: %w", ErrInvalid)
	}
	ref, err := c.store.StoreDetail(ctx, string(document.OrganizationID), raw)
	if err != nil {
		return Document{}, fmt.Errorf("store search content: %w", err)
	}
	if ref.Validate(string(document.OrganizationID)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 || ref.UncompressedSize != int64(len(raw)) {
		return Document{}, artifact.ErrIntegrity
	}
	verified, err := c.store.ReadDetail(ctx, string(document.OrganizationID), ref)
	if err != nil || !bytes.Equal(raw, verified) {
		return Document{}, errors.Join(artifact.ErrIntegrity, err)
	}
	document.Payload = ref
	document.Source = &Source{Vector: Vector(document, text), Statistics: Describe(document, text)}
	return document, nil
}

func (c *Content) Read(ctx context.Context, document Document) (Text, error) {
	if c.store == nil {
		return Text{}, artifact.ErrStorage
	}
	if document.Payload.Validate(string(document.OrganizationID)) != nil {
		return Text{}, artifact.ErrIntegrity
	}
	raw, err := c.store.ReadDetail(ctx, string(document.OrganizationID), document.Payload)
	if err != nil {
		return Text{}, fmt.Errorf("read search content: %w", errors.Join(artifact.ErrIntegrity, err))
	}
	var value payload
	if int64(len(raw)) != document.Payload.UncompressedSize || json.Unmarshal(raw, &value) != nil || !value.Document.UpdatedAt.Equal(document.UpdatedAt) {
		return Text{}, artifact.ErrIntegrity
	}
	document.Payload = attachment.ArchiveReference{}
	document.Generation, document.Placeholder = 0, false
	document.Source = nil
	value.Document.UpdatedAt = document.UpdatedAt
	if value.Document != document || strings.ReplaceAll(value.Text.Title, "\x00", "") != document.Title {
		return Text{}, artifact.ErrIntegrity
	}
	return boundedText(value.Text), nil
}

func (c *Content) RefreshOne(ctx context.Context, repository Repository, organization tracker.OrganizationID) (bool, error) {
	var document Document
	err := repository.ReadSearch(ctx, organization, corestore.Background, func(r Reader) error {
		var err error
		document, err = r.PendingSearch(ctx)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = repository.WriteSearch(ctx, organization, corestore.Background, func(w Writer) error { return w.AdvanceSearch(ctx, document) })
	if err != nil {
		return true, err
	}
	var text Text
	aggregateNeeded := false
	if document.Kind == "issue" {
		var progress Progress
		var more bool
		text, progress, more, err = c.refreshWorkText(ctx, repository, document)
		aggregateNeeded = true
		if err == nil && more {
			partial, prepareErr := c.Prepare(ctx, document, text)
			if prepareErr != nil {
				return true, prepareErr
			}
			progress.Payload = partial.Payload
			err = repository.WriteSearch(ctx, document.OrganizationID, corestore.Background, func(w Writer) error {
				_, err := w.SaveSearchProgress(ctx, document, progress)
				return err
			})
			return true, err
		}
	} else {
		text, err = c.Read(ctx, document)
	}
	if err != nil {
		return true, err
	}
	prepared := Prepared{Document: document, Vector: Vector(document, text), Literals: LiteralTerms(document, text), Statistics: Describe(document, text)}
	if aggregateNeeded {
		aggregate, err := c.Prepare(ctx, document, text)
		if err != nil {
			return true, err
		}
		prepared.Payload = aggregate.Payload
	}
	err = repository.WriteSearch(ctx, organization, corestore.Background, func(w Writer) error {
		_, err := w.PublishSearch(ctx, prepared)
		return err
	})
	return true, err
}

func bytePrefix(value string, budget int) string {
	if len(value) <= budget {
		return value
	}
	for budget > 0 && !utf8.RuneStart(value[budget]) {
		budget--
	}
	return value[:budget]
}

func boundedText(text Text) Text {
	text.Title = bytePrefix(text.Title, 64*1024)
	text.Body = bytePrefix(text.Body, 64*1024-len(text.Title))
	budget := 64*1024 - len(text.Title) - len(text.Body)
	labels := make([]string, 0, min(len(text.Labels), budget))
	for _, label := range text.Labels[:min(len(text.Labels), budget)] {
		if budget == 0 {
			break
		}
		label = bytePrefix(label, budget)
		budget -= len(label)
		if label != "" {
			labels = append(labels, label)
		}
	}
	text.Labels = labels
	return text
}
