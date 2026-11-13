package workduplicates

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Tenants         tenantstore.Repository
	Details         attachment.DetailStore
	Now             func() time.Time
	Logger          *slog.Logger
	AuthorizeRecord func(context.Context, accessstore.Reader, hubscope.NativeScope) error
	Read            func(context.Context, hubscope.NativeScope, string) (tracker.NativeIssue, error)
}

type Search struct {
	Available bool
	Sequence  int64
	Matches   []hubscope.PossibleDuplicate
}

func Prepare(ctx context.Context, deps Deps, scope hubscope.NativeScope, request tracker.CreateIssue, machine bool) (*Search, error) {
	text := request.Title + "\n" + request.Body
	if origin, stamped := issueorigin.Parse(request.Body); stamped {
		text = origin.Fingerprint + "\n" + issueorigin.DefectFingerprint(request.Body) + "\n" + text
	}
	query := searchstore.DuplicateSourceQuery{ProjectID: scope.Project, Machine: machine, Request: &searchstore.Text{Title: request.Title, Body: request.Body}, Text: BytePrefix(text, searchstore.MaxQueryTextBytes), Now: deps.Now()}
	if scope.Credential.HostedRole == "reporter" {
		query.Reporter = new(scope.Actor().PrincipalID)
	}
	var page searchstore.DuplicateSourcePage
	err := deps.Tenants.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := deps.AuthorizeRecord(ctx, r, scope); err != nil {
			return err
		}
		reader, ok := r.(searchstore.DuplicateSourceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		page, err = reader.DuplicateSources(ctx, query)
		return err
	})
	if errors.Is(err, searchstore.ErrIndexPending) || err == nil && !page.State.Ready {
		deps.Logger.InfoContext(ctx, "duplicate search unavailable; allowing canonical filing", "availability", "unavailable")
		return &Search{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(page.Sources) > 5 {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	content := searchstore.NewContent(deps.Details)
	var matches []searchstore.DuplicateMatch
	for _, source := range page.Sources {
		text, err := content.Read(ctx, source.Document)
		if err != nil {
			return nil, err
		}
		source.Statistics = searchstore.Describe(source.Document, text)
		parent := text
		if source.Document.Kind == "comment" || source.Document.Payload != source.Parent.Payload {
			parent, err = content.Read(ctx, source.Parent)
			if err != nil {
				return nil, err
			}
		}
		match, matched := searchstore.MatchDuplicateSource(source, text, parent, searchstore.Text{Title: request.Title, Body: request.Body})
		if matched && (!machine || match.Fingerprint) {
			matches = searchstore.MergeDuplicateMatches(matches, []searchstore.DuplicateMatch{match})
		}
	}
	search := &Search{Available: true, Sequence: page.State.Sequence}
	for _, match := range matches {
		source := match.Source
		item, err := deps.Read(ctx, scope, string(source.Document.WorkItemID))
		if err != nil {
			return nil, err
		}
		if item.Archived || item.Revision != source.Revision {
			return nil, hubapi.NativeConflict(item.Revision)
		}
		why := "Matching terms: " + strings.Join(match.Terms[:min(6, len(match.Terms))], ", ")
		if match.Fingerprint {
			why = "Matching machine fingerprint"
		}
		search.Matches = append(search.Matches, hubscope.PossibleDuplicate{WorkItemID: item.WorkItemID, Number: item.Number, Title: item.Title, State: item.State, Revision: item.Revision, Terminal: source.Terminal, Why: why, Identifier: string(scope.Project) + "#" + strconv.Itoa(item.Number)})
	}
	return search, nil
}

func BytePrefix(content string, budget int) string {
	if len(content) <= budget {
		return content
	}
	for budget > 0 && !utf8.RuneStart(content[budget]) {
		budget--
	}
	return content[:budget]
}
