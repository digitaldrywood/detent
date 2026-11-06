package searchstore

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type pagePosition struct {
	Version         int
	Fingerprint     [32]byte
	ID              string
	UpdatedAt       time.Time
	Coverage        int64
	Score           float64
	Number          int64
	NumberDelivered bool `json:",omitempty"`
	Exact           bool
	BatchID         string
	BatchAt         time.Time
	Scan            bool
	Streams         []StreamPosition
}

func (c *Content) Find(ctx context.Context, repository Repository, organization tracker.OrganizationID, query Query) (Page, error) {
	page := Page{Items: []Hit{}}
	if query.Limit < 1 || query.Limit > corestore.MaxPageSize {
		return page, ErrInvalid
	}
	if err := query.Validate(); err != nil {
		return page, err
	}
	position := pagePosition{}
	query.BeforeUpdatedAt, query.BeforeID, query.Streams = nil, "", nil
	fingerprintQuery := query
	fingerprintQuery.Cursor, fingerprintQuery.Limit = "", 0
	encoded, err := json.Marshal(struct {
		Organization tracker.OrganizationID
		Query        Query
	}{organization, fingerprintQuery})
	if err != nil {
		return page, ErrInvalid
	}
	fingerprint := sha256.Sum256(encoded)
	if query.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(query.Cursor)
		if err != nil || json.Unmarshal(raw, &position) != nil || position.Version != 4 || position.Fingerprint != fingerprint || len(position.Streams) > (TermLimit*2+2)*(len(query.ProjectIDs)+1)*LexemeLimit {
			return page, corestore.ErrInvalidCursor
		}
		query.Streams = position.Streams
		if position.BatchID != "" {
			query.BeforeUpdatedAt, query.BeforeID = new(position.BatchAt), position.BatchID
		} else if query.Kind == "thread" {
			query.BeforeUpdatedAt, query.BeforeID = new(position.UpdatedAt), position.ID
		}
	}
	var hits []Hit
	err = repository.ReadSearch(ctx, organization, corestore.Serving, func(r Reader) error { var err error; hits, err = r.Search(ctx, query); return err })
	if err != nil {
		return page, err
	}
	terms := Terms(query.Text)
	type candidate struct {
		hit         Hit
		text        *Text
		frequencies []int64
		tokens      int64
		exact       bool
	}
	candidates := make([]candidate, 0, len(hits))
	df := make([]int, len(terms))
	totalTokens := int64(0)
	var boundary *Document
	streams := slices.Clone(query.Streams)
	moreStreams := false
	for _, hit := range hits {
		if hit.Stream != nil {
			index := slices.IndexFunc(streams, func(p StreamPosition) bool { return p.Key == hit.Stream.Key && p.Lexeme == hit.Stream.Lexeme })
			if index < 0 {
				streams = append(streams, *hit.Stream)
			} else {
				streams[index] = *hit.Stream
			}
			moreStreams = moreStreams || !hit.Stream.Done
			continue
		}
		if hit.ScanBoundary {
			boundary = new(hit.Document)
			continue
		}
		value := candidate{hit: hit, frequencies: make([]int64, len(terms))}
		if query.Text == "" {
			candidates = append(candidates, value)
			continue
		}
		statistics := hit.Index
		if statistics == nil {
			text, err := c.readSearchText(ctx, repository, hit.Document)
			if err != nil {
				return page, err
			}
			value.text = new(searchableText(hit.Document, text))
			statistics = new(Describe(hit.Document, text))
		}
		value.tokens = statistics.Tokens
		for index, term := range terms {
			offset, found := slices.BinarySearch(statistics.Terms, term)
			if query.Exact {
				if found {
					value.frequencies[index] = statistics.Frequencies[offset]
				}
				continue
			}
			for offset < len(statistics.Terms) && strings.HasPrefix(statistics.Terms[offset], term) {
				value.frequencies[index] += statistics.Frequencies[offset]
				offset++
			}
		}
		value.hit.Coverage = 0
		for index, frequency := range value.frequencies {
			if frequency > 0 {
				value.hit.Coverage++
				df[index]++
			}
		}
		value.exact = query.Kind == "work" && numberMatch(query.Text, hit.Number, hit.Document.ProjectID, hit.ProjectName)
		literal := false
		if value.hit.Coverage == 0 && !value.exact && hit.Document.Kind == "issue" && !query.Exact {
			for _, project := range []string{hit.ProjectName, string(hit.Document.ProjectID)} {
				literal = literal || strings.Contains(lowerASCII(project+"#"+strconv.FormatInt(hit.Number, 10)), lowerASCII(query.Text))
			}
			if !literal && value.text == nil {
				text, err := c.Read(ctx, hit.Document)
				if err != nil {
					return page, err
				}
				value.text = new(text)
			}
			if value.text != nil {
				for _, content := range append([]string{value.text.Title, value.text.Body}, value.text.Labels...) {
					literal = literal || strings.Contains(lowerASCII(content), lowerASCII(query.Text))
				}
			}
		}
		if value.hit.Coverage == 0 && !literal && !value.exact {
			continue
		}
		totalTokens += value.tokens
		candidates = append(candidates, value)
	}
	slices.SortFunc(streams, func(a, b StreamPosition) int {
		return cmp.Or(strings.Compare(a.Key, b.Key), strings.Compare(b.Lexeme, a.Lexeme))
	})
	retained := streams[:0]
	for start := 0; start < len(streams); {
		end := start + 1
		for end < len(streams) && streams[end].Key == streams[start].Key {
			end++
		}
		retained = append(retained, streams[start:min(start+LexemeLimit, end)]...)
		start = end
	}
	streams = retained
	average := float64(totalTokens) / float64(max(1, len(candidates)))
	for i := range candidates {
		value := &candidates[i]
		value.hit.Score = 0
		for index, frequency := range value.frequencies {
			if frequency == 0 {
				continue
			}
			idf := math.Log((float64(len(candidates)-df[index]) + 0.5) / (float64(df[index]) + 0.5))
			idf = max(idf, 0.000001)
			f := float64(frequency)
			value.hit.Score -= idf * f * 2.2 / (f + 1.2*(0.25+0.75*float64(value.tokens)/max(1, average)))
		}
		value.hit.Score = -float64(value.hit.Coverage) + value.hit.Score/(1-value.hit.Score)
		if value.exact {
			value.hit.Score = -float64(TermLimit + 1)
		}
	}
	compare := func(a, b candidate) int {
		if query.Kind == "thread" {
			return cmp.Or(b.hit.Document.UpdatedAt.Compare(a.hit.Document.UpdatedAt), strings.Compare(b.hit.Document.ID, a.hit.Document.ID))
		}
		return cmp.Or(cmp.Compare(boolInt(b.exact), boolInt(a.exact)), cmp.Compare(b.hit.Coverage, a.hit.Coverage), cmp.Compare(a.hit.Score, b.hit.Score), cmp.Compare(b.hit.Number, a.hit.Number), strings.Compare(a.hit.Document.ID, b.hit.Document.ID))
	}
	slices.SortFunc(candidates, compare)
	for _, value := range candidates {
		if position.NumberDelivered && value.exact {
			continue
		}
		if query.Cursor != "" && !position.Scan && compare(value, candidate{hit: Hit{Document: Document{ID: position.ID, UpdatedAt: position.UpdatedAt}, Coverage: position.Coverage, Score: position.Score, Number: position.Number}, exact: position.Exact}) <= 0 {
			continue
		}
		if len(page.Items) == query.Limit {
			last := page.Items[len(page.Items)-1]
			raw, err := json.Marshal(pagePosition{Version: 4, Fingerprint: fingerprint, NumberDelivered: position.NumberDelivered, ID: last.Document.ID, UpdatedAt: last.Document.UpdatedAt, Coverage: last.Coverage, Score: last.Score, Number: last.Number, Exact: numberMatch(query.Text, last.Number, last.Document.ProjectID, last.ProjectName), BatchID: position.BatchID, BatchAt: position.BatchAt, Streams: position.Streams})
			if err != nil {
				return Page{}, ErrInvalid
			}
			page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
			break
		}
		if value.text == nil {
			var text Text
			var err error
			if value.hit.Index == nil {
				text, err = c.readSearchText(ctx, repository, value.hit.Document)
			} else {
				text, err = c.Read(ctx, value.hit.Document)
			}
			if err != nil {
				return Page{}, err
			}
			value.text = new(searchableText(value.hit.Document, text))
		}
		preview := []rune(snippet(*value.text, query.Text, query.Exact))
		start := 0
		if offset := slices.Index(preview, '['); offset >= 0 {
			start = max(0, offset-60)
		}
		value.hit.Snippet = string(preview[start:min(start+240, len(preview))])
		value.hit.Index = nil
		page.Items = append(page.Items, value.hit)
		position.NumberDelivered = position.NumberDelivered || value.exact
	}
	if page.NextCursor == "" && (boundary != nil || moreStreams) {
		next := pagePosition{Version: 4, Fingerprint: fingerprint, NumberDelivered: position.NumberDelivered, Scan: true, Streams: streams}
		if boundary != nil {
			next.BatchID, next.BatchAt = boundary.ID, boundary.UpdatedAt
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return Page{}, ErrInvalid
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func numberMatch(query string, number int64, project tracker.ProjectID, projectName string) bool {
	parsed, name := Number(query)
	return parsed > 0 && parsed == number && (name == "" || lowerASCII(name) == lowerASCII(string(project)) || lowerASCII(name) == lowerASCII(projectName))
}

func lowerASCII(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, value)
}
