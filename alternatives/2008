package hubserver

import (
	"strconv"
	"strings"
	"unicode"
)

func nativeSearchNumber(query string) (int, string) {
	project, number, qualified := strings.Cut(query, "#")
	if !qualified {
		number, project = project, ""
	}
	if strings.IndexFunc(number, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, ""
	}
	value, err := strconv.Atoi(number)
	if err != nil || value <= 0 {
		return 0, ""
	}
	return value, project
}

func nativeSearchExpressions(query string) []string {
	terms := strings.FieldsFunc(query, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	seen := map[string]bool{}
	var expressions []string
	for _, term := range terms {
		term = strings.ToLower(term)
		if !seen[term] {
			expressions = append(expressions, `"`+term+`"*`)
			seen[term] = true
		}
	}
	if len(expressions) == 0 {
		return []string{`""`}
	}
	return expressions
}

const nativeSearchJoin = ` LEFT JOIN (
WITH hits AS MATERIALIZED (
 SELECT d.issue_id, d.id, bm25(native_work_fts, 5.0, 1.0) AS score,
 snippet(native_work_fts, -1, '[', ']', ' … ', 32) AS snippet
 FROM native_work_fts JOIN native_work_documents d ON d.id = native_work_fts.rowid
 JOIN issues scoped ON scoped.id = d.issue_id
 WHERE native_work_fts MATCH ? AND scoped.organization_id = ? AND scoped.project_id = ?
), coverage AS MATERIALIZED (
 SELECT native_work_fts.rowid AS id, count(*) AS terms
 FROM json_each(?) term CROSS JOIN native_work_fts
 WHERE native_work_fts MATCH term.value AND EXISTS (SELECT 1 FROM hits WHERE hits.id = native_work_fts.rowid)
 GROUP BY native_work_fts.rowid
)
SELECT issue_id, score, substr(snippet, max(1, instr(snippet, '[') - 60), 240) AS snippet FROM (
 SELECT hits.issue_id, hits.snippet, -coverage.terms + hits.score / (1 - hits.score) AS score,
 row_number() OVER (PARTITION BY hits.issue_id ORDER BY coverage.terms DESC, hits.score, hits.id) AS hit
 FROM hits JOIN coverage ON coverage.id = hits.id
) WHERE hit = 1
) search ON search.issue_id = i.id `

const nativeLegacySearchText = `i.title || ' ' || i.body || ' ' || i.labels_json || ' ' ||
 (SELECT name FROM projects WHERE id = i.project_id) || '#' || i.number || ' ' || i.project_id || '#' || i.number`

const nativeNumberSearchJoin = ` LEFT JOIN (SELECT ? AS number, ? AS project, ? AS score) number_search
 ON i.number = number_search.number AND (number_search.project = '' OR lower(number_search.project) = lower(i.project_id)
 OR lower(number_search.project) = lower((SELECT name FROM projects WHERE id = i.project_id))) `

const nativeSearchScore = `COALESCE(number_search.score, search.score, 0)`

const nativeSearchProjection = `, ` + nativeSearchScore + ` AS search_score, COALESCE(search.snippet,
 substr(` + nativeLegacySearchText + `, max(1, instr(lower(` + nativeLegacySearchText + `), lower(?)) - 60), 240)) AS search_snippet`
