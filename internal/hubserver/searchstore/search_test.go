package searchstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type searchPopulation struct {
	Repository
	Reader
	attachment.DetailStore
	size int
}

func (p searchPopulation) document(number int) Document {
	id := fmt.Sprintf("item_%06d", number)
	return Document{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: tracker.NativeWorkItemID(id), ID: "issue/" + id, SourceID: id, Kind: "issue", Scope: "issue", Revision: 1, Title: "sentinel", UpdatedAt: time.Unix(1, 0).UTC()}
}

func (p searchPopulation) ReadDetail(_ context.Context, _ string, ref attachment.ArchiveReference) ([]byte, error) {
	number, err := strconv.ParseInt(ref.SHA256, 16, 64)
	if err != nil {
		return nil, err
	}
	return json.Marshal(payload{Document: p.document(int(number)), Text: Text{Title: "sentinel"}})
}

func (p searchPopulation) ReadSearch(ctx context.Context, _ tracker.OrganizationID, _ corestore.Workload, read func(Reader) error) error {
	return read(p)
}

func (p searchPopulation) Search(_ context.Context, query Query) ([]Hit, error) {
	start := p.size
	if len(query.Streams) > 0 {
		var err error
		start, err = strconv.Atoi(query.Streams[0].ID)
		if err != nil {
			return nil, err
		}
		start--
	}
	end := max(1, start-CandidateLimit+1)
	hits := make([]Hit, 0, CandidateLimit+1)
	for number := start; number >= end; number-- {
		d := p.document(number)
		raw, err := json.Marshal(payload{Document: d, Text: Text{Title: "sentinel"}})
		if err != nil {
			return nil, err
		}
		digest := fmt.Sprintf("%064x", number)
		key, err := attachment.ArchiveKey("org_example", digest)
		if err != nil {
			return nil, err
		}
		d.Payload = attachment.ArchiveReference{Key: key, SHA256: digest, CompressedSize: 1, UncompressedSize: int64(len(raw)), Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
		hits = append(hits, Hit{Document: d, Number: int64(number), Index: &Statistics{Tokens: 1, Terms: []string{"sentinel"}, Frequencies: []int64{5}}})
	}
	if end > 1 {
		hits = append(hits, Hit{Stream: &StreamPosition{Key: "population:sentinel", Lexeme: "sentinel", ID: strconv.Itoa(end), Impact: 1}})
	}
	return hits, nil
}

func TestSearchReaches128001WorkItemsWithConstantCursor(t *testing.T) {
	const count = 128001
	population := searchPopulation{size: count}
	content := NewContent(population)
	query := Query{Kind: "work", ProjectID: "prj_example", Text: "sentinel", Exact: true, Limit: corestore.MaxPageSize}
	seen := 0
	for pageNumber := 0; pageNumber < count; pageNumber++ {
		page, err := content.Find(t.Context(), population, "org_example", query)
		if err != nil {
			t.Fatalf("after %d matches: %v", seen, err)
		}
		for _, hit := range page.Items {
			want := count - seen
			if hit.Number != int64(want) {
				t.Fatalf("match %d number=%d want=%d", seen, hit.Number, want)
			}
			seen++
		}
		if len(page.NextCursor) > 2048 {
			t.Fatalf("cursor grew to %d bytes after %d matches", len(page.NextCursor), seen)
		}
		if page.NextCursor == "" {
			if seen != count {
				t.Fatalf("reached %d of %d matches", seen, count)
			}
			return
		}
		if page.NextCursor == query.Cursor || len(page.Items) == 0 {
			t.Fatalf("pagination stopped after %d matches", seen)
		}
		query.Cursor = page.NextCursor
	}
	t.Fatalf("search did not exhaust after %d matches", seen)
}
