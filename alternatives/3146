package searchstore

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (c *Content) readSearchText(ctx context.Context, repository Repository, d Document) (Text, error) {
	if d.Kind != "issue" {
		return c.Read(ctx, d)
	}
	text, _, err := c.readWorkText(ctx, repository, d, corestore.Serving)
	return text, err
}

func (c *Content) readWorkText(ctx context.Context, repository Repository, d Document, workload corestore.Workload) (Text, bool, error) {
	changed := d.Placeholder
	text := Text{Title: d.Title}
	if !d.Placeholder {
		var err error
		text, err = c.Read(ctx, d)
		if err != nil {
			return Text{}, false, err
		}
	}
	text = boundedText(text)
	budget := 64*1024 - len(text.Title) - len(text.Body)
	for _, label := range text.Labels {
		budget -= len(label)
	}
	var body strings.Builder
	body.Grow(len(text.Body) + budget)
	body.WriteString(text.Body)
	after := ""
	sourcesRead := 0
	for budget > 1 {
		if sourcesRead >= 32 {
			return Text{}, false, ErrIndexPending
		}
		var sources []Document
		err := repository.ReadSearch(ctx, d.OrganizationID, workload, func(r Reader) error {
			var err error
			sources, err = r.SearchSources(ctx, d, after, min(32, budget-1))
			return err
		})
		if err != nil {
			return Text{}, false, err
		}
		if len(sources) == 0 {
			break
		}
		for _, source := range sources {
			sourcesRead++
			changed = true
			value, err := c.Read(ctx, source)
			if err != nil {
				return Text{}, false, err
			}
			content := strings.Join(append([]string{value.Title, value.Body}, value.Labels...), " ")
			content = bytePrefix(content, budget-1)
			body.WriteByte('\n')
			body.WriteString(content)
			budget -= 1 + len(content)
			after = source.ID
			if budget <= 1 {
				break
			}
		}
	}
	text.Body = body.String()
	return text, changed, nil
}

func (c *Content) refreshWorkText(ctx context.Context, repository Repository, d Document) (Text, Progress, bool, error) {
	var progress Progress
	err := repository.ReadSearch(ctx, d.OrganizationID, corestore.Background, func(r Reader) error { var err error; progress, err = r.SearchProgress(ctx, d); return err })
	if err != nil {
		return Text{}, Progress{}, false, err
	}
	text := Text{Title: d.Title}
	if progress.Payload.Key != "" {
		partial := d
		partial.Payload = progress.Payload
		text, err = c.Read(ctx, partial)
	} else if !d.Placeholder {
		text, err = c.Read(ctx, d)
	}
	if err != nil {
		return Text{}, Progress{}, false, err
	}
	text = boundedText(text)
	budget := 64*1024 - len(text.Title) - len(text.Body)
	for _, label := range text.Labels {
		budget -= len(label)
	}
	if budget <= 1 {
		return text, progress, false, nil
	}
	var sources []Document
	err = repository.ReadSearch(ctx, d.OrganizationID, corestore.Background, func(r Reader) error {
		var err error
		sources, err = r.SearchSources(ctx, d, progress.AfterID, min(32, budget-1))
		return err
	})
	if err != nil {
		return Text{}, Progress{}, false, err
	}
	var body strings.Builder
	body.Grow(len(text.Body) + budget)
	body.WriteString(text.Body)
	for _, source := range sources {
		value, err := c.Read(ctx, source)
		if err != nil {
			return Text{}, Progress{}, false, err
		}
		content := strings.Join(append([]string{value.Title, value.Body}, value.Labels...), " ")
		content = bytePrefix(content, budget-1)
		body.WriteByte('\n')
		body.WriteString(content)
		budget -= 1 + len(content)
		progress.AfterID = source.ID
		if budget <= 1 {
			break
		}
	}
	text.Body = body.String()
	return text, progress, len(sources) == 32 && budget > 1, nil
}
