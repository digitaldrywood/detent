package searchstore

import (
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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
	if strings.ContainsRune(project, 0) || strings.IndexFunc(number, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
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

var vectorQuote = strings.NewReplacer("\\", "\\\\", "'", "''")

func quote(value string) string {
	return "'" + vectorQuote.Replace(value) + "'"
}

func indexedTerm(term string) string {
	budget := maxLexemeBytes - 1
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
			frequencies[indexedTerm(token.term)] += weight
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

func lowerASCII(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, value)
}
