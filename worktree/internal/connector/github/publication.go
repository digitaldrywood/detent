package github

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/publication"
)

func (c *Connector) protectPublicationText(ctx context.Context, repository string, text string) string {
	if c == nil || len(c.publication.Sources) == 0 || text == "" {
		return text
	}
	repository = strings.TrimSpace(repository)
	if repository == "" {
		repository = c.publication.DestinationRepository
	}
	policy := c.publication
	policy.DestinationRepository = repository
	policy.Visibility = c.publicationVisibilityFor(ctx, repository)
	return publication.Protect(text, policy)
}

func (c *Connector) publicationVisibilityFor(ctx context.Context, repository string) publication.Visibility {
	c.publicationMu.Lock()
	defer c.publicationMu.Unlock()
	if visibility, ok := c.publicationVisibilities[strings.ToLower(repository)]; ok {
		return visibility
	}
	visibility := publication.VisibilityUnknown
	info, err := c.FetchRepositoryInfo(ctx, repository)
	if err == nil {
		switch {
		case info.Private || strings.EqualFold(info.Visibility, string(publication.VisibilityPrivate)) || strings.EqualFold(info.Visibility, "internal"):
			visibility = publication.VisibilityPrivate
		case strings.EqualFold(info.Visibility, string(publication.VisibilityPublic)):
			visibility = publication.VisibilityPublic
		}
	}
	if c.publicationVisibilities == nil {
		c.publicationVisibilities = map[string]publication.Visibility{}
	}
	c.publicationVisibilities[strings.ToLower(repository)] = visibility
	return visibility
}
