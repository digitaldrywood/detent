package searchstore

import (
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const maxLexemeBytes = 2046

type token struct {
	term       string
	start, end int
}

func Number(query string) (int64, string) {
	project, number, qualified := strings.Cut(query, "#")
	if !qualified {
		number, project = project, ""
	}
	if strings.IndexFunc(number, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, ""
	}
	value, err := strconv.ParseInt(number, 10, 64)
	if err != nil || value <= 0 {
		return 0, ""
	}
	return value, project
}

func normalize(value string) string {
	var out strings.Builder
	for _, r := range value {
		index, found := slices.BinarySearchFunc(foldings, r, func(pair [2]rune, value rune) int { return int(pair[0] - value) })
		if found {
			r = foldings[index][1]
		}
		if r != 0 {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func tokenize(value string) []token {
	var tokens []token
	start := -1
	for offset, r := range value {
		if r == 0xfffe || r == 0xffff {
			r = utf8.RuneError
		}
		index, _ := slices.BinarySearch(tokenBoundaries, r+1)
		isToken := index%2 == 1
		if start >= 0 && diacritic(r) {
			isToken = true
		}
		if isToken && start < 0 {
			start = offset
		}
		if !isToken && start >= 0 {
			tokens = append(tokens, token{stem(normalize(value[start:offset])), start, offset})
			start = -1
		}
	}
	if start >= 0 {
		tokens = append(tokens, token{stem(normalize(value[start:])), start, len(value)})
	}
	return tokens
}

func Terms(query string) []string {
	parts := strings.FieldsFunc(query, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	seen := make(map[string]bool)
	var terms []string
	for _, part := range parts {
		part = strings.ToLower(part)
		if seen[part] {
			continue
		}
		seen[part] = true
		terms = append(terms, stem(normalize(part)))
		if len(terms) == TermLimit {
			break
		}
	}
	return terms
}

func Prefix(organization tracker.OrganizationID, project tracker.ProjectID) string {
	return "s" + hex.EncodeToString([]byte(organization)) + "z" + hex.EncodeToString([]byte(project)) + "_"
}

func quote(value string) string {
	return "'" + strings.NewReplacer("\\", "\\\\", "'", "''").Replace(value) + "'"
}

func Expressions(organization tracker.OrganizationID, query Query) []string {
	prefix := Prefix(organization, query.ProjectID)
	terms := Terms(query.Text)
	for i, term := range terms {
		terms[i] = quote(prefix + "w" + term)
		if !query.Exact {
			terms[i] += ":*"
		}
	}
	return terms
}

func grams(value string) []string {
	runes := []rune(lowerASCII(value))
	size := min(3, len(runes))
	if size == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var result []string
	for i := 0; i+size <= len(runes); i++ {
		term := hex.EncodeToString([]byte(string(runes[i : i+size])))
		if !seen[term] {
			result = append(result, term)
			seen[term] = true
		}
	}
	return result
}

func LiteralExpression(organization tracker.OrganizationID, query Query) string {
	prefix := Prefix(organization, query.ProjectID)
	terms := grams(query.Text)
	for i, term := range terms {
		terms[i] = quote(prefix + "g" + term)
	}
	return strings.Join(terms, " & ")
}

func Vector(document Document, text Text) string {
	text = searchableText(document, text)
	prefixes := []string{Prefix(document.OrganizationID, "")}
	if document.ProjectID != "" {
		prefixes = append(prefixes, Prefix(document.OrganizationID, document.ProjectID))
	}

	type entry struct {
		term      string
		positions []string
		last      int
	}
	entries := make([]entry, 0)
	offsets := make(map[string]int)
	size := 4
	complete := true
	add := func(term string, position int, weight string) {
		if len(term) > maxLexemeBytes {
			complete = false
			return
		}
		index, exists := offsets[term]
		if !exists {
			if size+len(term)+16 >= 1024*1024 {
				complete = false
				return
			}
			offsets[term] = len(entries)
			index = len(entries)
			entries = append(entries, entry{term: term})
			size += len(term) + 12
		}
		value := &entries[index]
		if position == 0 || len(value.positions) == 256 || position == value.last || size+4 >= 1024*1024 {
			return
		}
		value.positions = append(value.positions, strconv.Itoa(position)+weight)
		value.last = position
		size += 2
	}
	add("~complete", 0, "")
	for column, content := range []string{text.Title, text.Body} {
		weight := "D"
		if column == 0 {
			weight = "A"
		}
		for _, prefix := range prefixes {
			position := 0
			for _, token := range tokenize(content) {
				position++
				add(prefix+"w"+indexedTerm(document, token.term), min(position, 16383), weight)
			}
		}
	}
	if document.Kind == "issue" || document.Kind == "comment" {
		for _, prefix := range prefixes {
			columns := []string{text.Title, text.Body}
			if document.Kind == "issue" {
				columns = append(columns, text.Labels...)
			}
			for _, column := range columns {
				runes := []rune(column)
				for size := 1; size <= 3; size++ {
					for i := 0; i+size <= len(runes); i++ {
						term := prefix + "g" + hex.EncodeToString([]byte(lowerASCII(string(runes[i:i+size]))))
						add(term, 0, "")
					}
				}
			}
		}
	}
	var vector strings.Builder
	for _, entry := range entries {
		if entry.term == "~complete" && !complete {
			continue
		}
		vector.WriteString(quote(entry.term))
		if len(entry.positions) > 0 {
			vector.WriteByte(':')
			vector.WriteString(strings.Join(entry.positions, ","))
		}
		vector.WriteByte(' ')
	}
	return vector.String()
}

func indexedTerm(document Document, term string) string {
	budget := max(0, maxLexemeBytes-len(Prefix(document.OrganizationID, document.ProjectID))-1)
	if len(term) > budget {
		return bytePrefix(term, max(0, budget-1)) + "~"
	}
	return term
}

func Describe(document Document, text Text) Statistics {
	text = searchableText(document, text)
	statistics := Statistics{}
	frequencies := make(map[string]int64)
	for column, content := range []string{text.Title, text.Body} {
		weight := int64(1)
		if column == 0 {
			weight = 5
		}
		for _, token := range tokenize(content) {
			statistics.Tokens++
			frequencies[indexedTerm(document, token.term)] += weight
		}
	}
	statistics.Terms = make([]string, 0, len(frequencies))
	for term := range frequencies {
		statistics.Terms = append(statistics.Terms, term)
	}
	slices.Sort(statistics.Terms)
	statistics.Frequencies = make([]int64, len(statistics.Terms))
	for i, term := range statistics.Terms {
		statistics.Frequencies[i] = frequencies[term]
	}
	return statistics
}

func searchableText(document Document, text Text) Text {
	text = boundedText(text)
	if document.Kind == "thread" {
		text.Body, text.Labels = "", nil
	}
	return text
}

func Snippet(text Text, query string) string {
	return snippet(text, query, false)
}

func snippet(text Text, query string, exact bool) string {
	terms := Terms(query)
	type span struct {
		token
		terms []string
	}
	content, coverage, anchor, count := text.Title, -1, 0, 0
	var matches []span
	for _, column := range []string{text.Title, text.Body} {
		var spans []span
		for _, token := range tokenize(column) {
			var matched []string
			for _, term := range terms {
				if token.term == term || !exact && strings.HasPrefix(token.term, term) {
					matched = append(matched, term)
				}
			}
			if len(matched) > 0 {
				spans = append(spans, span{token, matched})
			}
		}
		if coverage < 0 {
			content, coverage = column, 0
		}
		covered := make(map[string]int)
		right := 0
		for left, match := range spans {
			for right < len(spans) && (right == left || spans[right].end-match.start <= 180) {
				for _, term := range spans[right].terms {
					covered[term]++
				}
				right++
			}
			if len(covered) > coverage || len(covered) == coverage && right-left > count {
				content, matches, coverage, anchor, count = column, spans, len(covered), match.start, right-left
			}
			for _, term := range match.terms {
				covered[term]--
				if covered[term] == 0 {
					delete(covered, term)
				}
			}
		}
	}
	start := 0
	if len(matches) > 0 {
		start = max(0, anchor-60)
		for start > 0 && !utf8.RuneStart(content[start]) {
			start--
		}
	}
	if len(matches) == 0 && query != "" {
		plain := strings.Join(append([]string{text.Title, text.Body}, text.Labels...), " ")
		if offset := strings.Index(lowerASCII(plain), lowerASCII(query)); offset >= 0 {
			content = plain
			start = max(0, offset-60)
			for start > 0 && !utf8.RuneStart(content[start]) {
				start--
			}
		}
	}
	runes := []rune(content[start:])
	end := start + len(string(runes[:min(240, len(runes))]))
	var out strings.Builder
	for _, match := range matches {
		if match.start < start || match.end > end {
			continue
		}
		out.WriteString(content[start:match.start])
		out.WriteByte('[')
		out.WriteString(content[match.start:match.end])
		out.WriteByte(']')
		start = match.end
	}
	out.WriteString(content[start:end])
	return out.String()
}

func CandidateTerms(organization tracker.OrganizationID, query Query) []string {
	prefix := Prefix(organization, query.ProjectID)
	terms := Terms(query.Text)
	result := make([]string, 0, len(terms)+TermLimit)
	for _, term := range terms {
		if len(prefix)+1+len(term) < 2048 {
			result = append(result, prefix+"w"+term)
		}
	}
	if query.Kind == "work" && !query.Exact {
		literals := grams(query.Text)
		for _, term := range literals[:min(len(literals), TermLimit)] {
			result = append(result, prefix+"g"+term)
		}
	}
	return result
}

func LiteralPostings(document Document, text Text) []LiteralPosting {
	if document.Kind != "issue" && document.Kind != "comment" {
		return nil
	}
	text = boundedText(text)
	prefixes := []string{Prefix(document.OrganizationID, "")}
	if document.ProjectID != "" {
		prefixes = append(prefixes, Prefix(document.OrganizationID, document.ProjectID))
	}
	positions := make(map[string][]int64)
	columns := []string{text.Title, text.Body}
	if document.Kind == "issue" {
		columns = append(columns, text.Labels...)
	}
	for field, column := range columns {
		runes := []rune(lowerASCII(column))
		for size := 1; size <= 3; size++ {
			for i := 0; i+size <= len(runes); i++ {
				gram := hex.EncodeToString([]byte(string(runes[i : i+size])))
				for prefixIndex, prefix := range prefixes {
					term := prefix + "g" + gram
					if len(term) >= 2048 {
						continue
					}
					if size == 3 && document.ProjectID != "" && prefixIndex == len(prefixes)-1 {
						positions[term] = append(positions[term], int64(field)<<32|int64(i))
					} else if _, ok := positions[term]; !ok {
						positions[term] = nil
					}
				}
			}
		}
	}
	terms := make([]string, 0, len(positions))
	for term := range positions {
		terms = append(terms, term)
	}
	slices.Sort(terms)
	postings := make([]LiteralPosting, len(terms))
	for index, term := range terms {
		postings[index] = LiteralPosting{Term: term, Positions: positions[term]}
	}
	return postings
}

func LiteralQueryTerms(organization tracker.OrganizationID, project tracker.ProjectID, query string) []string {
	runes := []rune(lowerASCII(query))
	size := min(3, len(runes))
	if size == 0 {
		return nil
	}
	prefix := Prefix(organization, project) + "g"
	terms := make([]string, 0, len(runes)-size+1)
	for index := 0; index+size <= len(runes); index++ {
		terms = append(terms, prefix+hex.EncodeToString([]byte(string(runes[index:index+size]))))
	}
	return terms
}

func LiteralHex(value string) string {
	return hex.EncodeToString([]byte(lowerASCII(value)))
}
