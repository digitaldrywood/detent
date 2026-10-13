package hubserver

import (
	"strings"
	"unicode"
)

func nativeSearchExpression(query string) string {
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
		return `""`
	}
	return strings.Join(expressions, " OR ")
}

const nativeSearchJoin = ` LEFT JOIN (
WITH hits AS MATERIALIZED (
 SELECT d.issue_id, d.id, bm25(native_work_fts, 5.0, 1.0) AS score,
 snippet(native_work_fts, -1, '[', ']', ' … ', 32) AS snippet
 FROM native_work_fts JOIN native_work_documents d ON d.id = native_work_fts.rowid
 JOIN issues scoped ON scoped.id = d.issue_id
 WHERE native_work_fts MATCH ? AND scoped.organization_id = ? AND scoped.project_id = ?
)
SELECT issue_id, score, substr(snippet, max(1, instr(snippet, '[') - 60), 240) AS snippet FROM (
 SELECT *, row_number() OVER (PARTITION BY issue_id ORDER BY score, id) AS hit FROM hits
) WHERE hit = 1
) search ON search.issue_id = i.id `

const nativeLegacySearchText = `i.title || ' ' || i.body || ' ' || i.labels_json || ' ' ||
 (SELECT name FROM projects WHERE id = i.project_id) || '#' || i.number || ' ' || i.project_id || '#' || i.number`

const nativeSearchProjection = `, COALESCE(search.score, 0), COALESCE(search.snippet,
 substr(` + nativeLegacySearchText + `, max(1, instr(lower(` + nativeLegacySearchText + `), lower(?)) - 60), 240))`
