package searchstore

import (
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type DuplicateMatch struct {
	Source      DuplicateSource
	Score       float64
	Terms       []string
	Fingerprint bool
}

func MatchDuplicateSource(source DuplicateSource, text Text, parent Text, request Text) (DuplicateMatch, bool) {
	source.Statistics = Describe(source.Document, text)
	match := DuplicateMatch{Source: source}
	if source.Statistics.Validate() != nil {
		return match, false
	}
	previous := text.Body
	if source.Document.Payload != source.Parent.Payload {
		previous = parent.Body
	}
	if source.Document.Kind == "comment" {
		previous = parent.Body + "\n" + text.Body
	}
	if _, stamped := issueorigin.Parse(request.Body); stamped && issueorigin.SameDefect(previous, request.Body) {
		match.Fingerprint = true
		return match, true
	}
	for column, content := range []string{request.Title, request.Body} {
		terms := duplicateTerms(content)
		if len(terms) == 0 {
			continue
		}
		minimum := max(2, (len(terms)*3+4)/5)
		if column == 0 && len(terms) == 1 {
			minimum = 1
		}
		matched := make([]string, 0, len(terms))
		frequency := int64(0)
		for _, term := range terms {
			found := false
			for i, sourceTerm := range source.Statistics.Terms {
				if strings.HasPrefix(sourceTerm, term) {
					frequency += source.Statistics.Frequencies[i]
					found = true
				}
			}
			if found {
				matched = append(matched, term)
			}
		}
		if len(matched) < minimum {
			continue
		}
		score := float64(frequency) / float64(1+source.Statistics.Tokens)
		if score > match.Score {
			match.Score = score
			match.Terms = matched
		}
	}
	return match, match.Score > 0
}

func duplicateTerms(content string) []string {
	var terms []string
	for _, term := range Terms(content) {
		if len(term) < 3 || len(term) > 64 || strings.Contains(" a an and are as at be been but by for from has have how if in into is it its not of on or our that the their then these this to was we were what when where which who will with would you your ", " "+term+" ") {
			continue
		}
		terms = append(terms, term)
	}
	return terms
}

func MergeDuplicateMatches(previous, next []DuplicateMatch) []DuplicateMatch {
	matches := slices.Clone(previous)
	for _, candidate := range next {
		position := slices.IndexFunc(matches, func(value DuplicateMatch) bool {
			return value.Source.Document.WorkItemID == candidate.Source.Document.WorkItemID
		})
		if position < 0 {
			matches = append(matches, candidate)
		} else if compareDuplicateMatch(candidate, matches[position]) < 0 {
			matches[position] = candidate
		}
	}
	slices.SortFunc(matches, compareDuplicateMatch)
	return matches[:min(5, len(matches))]
}

func compareDuplicateMatch(a, b DuplicateMatch) int {
	if a.Fingerprint != b.Fingerprint {
		if a.Fingerprint {
			return -1
		}
		return 1
	}
	if a.Fingerprint {
		if a.Source.Terminal != b.Source.Terminal {
			if a.Source.Terminal {
				return 1
			}
			return -1
		}
	} else if a.Score != b.Score {
		if a.Score > b.Score {
			return -1
		}
		return 1
	}
	if a.Source.Terminal != b.Source.Terminal {
		if a.Source.Terminal {
			return 1
		}
		return -1
	}
	if a.Source.Number > b.Source.Number {
		return -1
	}
	if a.Source.Number < b.Source.Number {
		return 1
	}
	return strings.Compare(a.Source.Document.ID, b.Source.Document.ID)
}

func MatchDuplicateLiteral(text Text, query string) bool {
	if query == "" {
		return false
	}
	query = lowerASCII(query)
	return strings.Contains(lowerASCII(text.Title), query) || strings.Contains(lowerASCII(text.Body), query)
}

func DuplicateQueryExpressions(organization tracker.OrganizationID, project tracker.ProjectID, content string) []string {
	terms := duplicateTerms(content)
	prefix := Prefix(organization, project)
	for i, term := range terms {
		terms[i] = quote(prefix+"w"+term) + ":*"
	}
	return terms
}
