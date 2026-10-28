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

const (
	nativeSearchCandidateLimit = 100
	nativeSearchTermGroupLimit = 100
)

func nativeSearchJoin(selection string, selectionArgs []any, expressions []string) (string, []any) {
	candidate := `SELECT d.issue_id,d.id,
-1.0-4.0*(length(highlight(native_work_fts,0,char(1),''))-length(native_work_fts.title)) AS score
FROM native_work_fts CROSS JOIN native_work_documents d CROSS JOIN eligible i
WHERE native_work_fts MATCH ? AND d.id=native_work_fts.rowid AND i.id=d.issue_id
ORDER BY native_work_fts.rowid DESC LIMIT ?`
	ctes := []string{"eligible AS NOT MATERIALIZED (" + strings.Replace(selection, "SELECT i.native_id", "SELECT i.id", 1) + ")"}
	var terms []string
	args := append([]any(nil), selectionArgs...)
	for index, expression := range expressions {
		name := "term" + strconv.Itoa(index)
		ctes = append(ctes, name+" AS MATERIALIZED ("+candidate+")")
		terms = append(terms, "SELECT issue_id,id,score FROM "+name)
		args = append(args, expression, nativeSearchCandidateLimit)
	}
	var groups []string
	for start := 0; start < len(terms); start += nativeSearchTermGroupLimit {
		name := "term_group" + strconv.Itoa(start/nativeSearchTermGroupLimit)
		ctes = append(ctes, name+" AS MATERIALIZED ("+strings.Join(terms[start:min(start+nativeSearchTermGroupLimit, len(terms))], " UNION ALL ")+")")
		groups = append(groups, "SELECT issue_id,id,score FROM "+name)
	}
	coverage := `SELECT issue_id,id,sum(score) AS score,count(*) AS terms FROM (` + strings.Join(groups, " UNION ALL ") + `) GROUP BY issue_id,id`
	if len(expressions) > 1 {
		ctes = append(ctes, "full_hits AS MATERIALIZED ("+candidate+")")
		args = append(args, strings.Join(expressions, " AND "), nativeSearchCandidateLimit)
		coverage += ` UNION ALL SELECT issue_id,id,score,? FROM full_hits`
		args = append(args, len(expressions))
	}
	args = append(args, strings.Join(expressions, " OR "))
	return ` LEFT JOIN (
WITH ` + strings.Join(ctes, ",") + `, coverage AS MATERIALIZED (` + coverage + `), ranked AS MATERIALIZED (
 SELECT issue_id,id,-terms+score/(1-score) AS score,
 row_number() OVER (PARTITION BY issue_id ORDER BY terms DESC,score,id) AS hit FROM coverage
), previews AS MATERIALIZED (
 SELECT ranked.issue_id,ranked.score,snippet(native_work_fts,-1,'[',']',' … ',32) AS preview
 FROM ranked CROSS JOIN native_work_fts
 WHERE ranked.hit=1 AND native_work_fts.rowid=ranked.id AND native_work_fts MATCH ?
)
SELECT issue_id,score,substr(preview,max(1,instr(preview,'[')-60),240) AS snippet FROM previews
) search ON search.issue_id=i.id `, args
}

const nativeLegacySearchText = `i.title || ' ' || i.body || ' ' || i.labels_json || ' ' ||
 (SELECT name FROM projects WHERE id = i.project_id) || '#' || i.number || ' ' || i.project_id || '#' || i.number`

const nativeNumberSearchJoin = ` LEFT JOIN (SELECT ? AS number, ? AS project, ? AS score) number_search
 ON i.number = number_search.number AND (number_search.project = '' OR lower(number_search.project) = lower(i.project_id)
 OR lower(number_search.project) = lower((SELECT name FROM projects WHERE id = i.project_id))) `

const nativeSearchScore = `COALESCE(number_search.score, search.score, 0)`

const nativeSearchProjection = `, ` + nativeSearchScore + ` AS search_score, COALESCE(search.snippet,
 substr(` + nativeLegacySearchText + `, max(1, instr(lower(` + nativeLegacySearchText + `), lower(?)) - 60), 240)) AS search_snippet`
