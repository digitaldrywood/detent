package hubserver

import (
	"encoding/hex"
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

func nativeSearchScopes(scope nativeScope, archived string) []string {
	prefix := "s" + strings.ToUpper(hex.EncodeToString([]byte(scope.organization))) + "z" + strings.ToUpper(hex.EncodeToString([]byte(scope.project)))
	suffix := "v"
	if scope.credential.HostedRole == "reporter" {
		principal := scope.actor().PrincipalID
		if principal == "" {
			return []string{"unavailable"}
		}
		suffix = "r" + strings.ToUpper(hex.EncodeToString([]byte(principal))) + "v"
	}
	switch archived {
	case "all":
		return []string{prefix + "a0" + suffix, prefix + "a1" + suffix}
	case "true":
		return []string{prefix + "a1" + suffix}
	default:
		return []string{prefix + "a0" + suffix}
	}
}

func nativeScopedExpression(scopes, expressions []string, operator string) string {
	var variants []string
	for _, scope := range scopes {
		var phrases []string
		for _, expression := range expressions {
			if expression == `""` {
				phrases = append(phrases, expression)
			} else {
				phrases = append(phrases, `"`+scope+` `+strings.TrimPrefix(expression, `"`))
			}
		}
		variants = append(variants, `scope : "`+scope+`" AND {title body} : (`+strings.Join(phrases, operator)+`)`)
	}
	return "(" + strings.Join(variants, " OR ") + ")"
}

func nativeSearchHits(scope nativeScope, archived string, expressions []string) (string, []any) {
	scopes := nativeSearchScopes(scope, archived)
	pending, args := nativeSearchIndexPending(scope)
	args = append(args, scope.organization, scope.project)
	visibility, visibilityArgs := scope.issueVisibilitySQL("i")
	args = append(args, visibilityArgs...)
	archiveFilter := " AND i.archived=0"
	switch archived {
	case "true":
		archiveFilter = " AND i.archived=1"
	case "all":
		archiveFilter = ""
	}
	args = append(args, nativeScopedExpression(scopes, expressions, " OR "), strings.Join(expressions, " OR "))
	values := make([]string, len(expressions))
	for i, expression := range expressions {
		values[i] = "(?,?)"
		args = append(args, expression, nativeScopedExpression(scopes, []string{expression}, " OR "))
	}
	return `WITH index_state AS MATERIALIZED (SELECT ` + pending + ` AS incomplete), fallback_issues AS MATERIALIZED (
 SELECT i.id FROM index_state CROSS JOIN issues i
 WHERE index_state.incomplete AND i.organization_id=? AND i.project_id=? AND (` + visibility + `)` + archiveFilter + `
 ), hits AS MATERIALIZED (
 SELECT d.issue_id,native_work_scoped_fts.rowid AS id,bm25(native_work_scoped_fts,5.0,1.0,0.0) AS score
 FROM index_state CROSS JOIN native_work_scoped_fts CROSS JOIN native_work_documents d
 WHERE NOT index_state.incomplete AND native_work_scoped_fts MATCH ? AND d.id=native_work_scoped_fts.rowid/2
 UNION ALL
 SELECT i.id,d.id*2,bm25(native_work_fts,5.0,1.0)
 FROM fallback_issues i CROSS JOIN native_work_documents d CROSS JOIN native_work_fts
 WHERE d.issue_id=i.id AND native_work_fts.rowid=d.id AND native_work_fts MATCH ?
 ), terms(matched,expression) AS (VALUES ` + strings.Join(values, ",") + `), coverage AS MATERIALIZED (
 SELECT native_work_scoped_fts.rowid AS id,count(*) AS terms,group_concat(terms.matched,', ') AS matched
 FROM index_state CROSS JOIN terms CROSS JOIN native_work_scoped_fts
 WHERE NOT index_state.incomplete AND native_work_scoped_fts MATCH terms.expression GROUP BY native_work_scoped_fts.rowid
 UNION ALL
 SELECT d.id*2,count(*),group_concat(terms.matched,', ')
 FROM fallback_issues i CROSS JOIN native_work_documents d CROSS JOIN terms CROSS JOIN native_work_fts
 WHERE d.issue_id=i.id AND native_work_fts.rowid=d.id AND native_work_fts MATCH terms.matched GROUP BY d.id
 ), ranked AS (
 SELECT hits.*,coverage.terms,coverage.matched,-coverage.terms+hits.score/(1-hits.score) AS search_score,
 row_number() OVER (PARTITION BY hits.issue_id ORDER BY coverage.terms DESC,hits.score,hits.id) AS hit
 FROM hits JOIN coverage ON coverage.id=hits.id
 ) `, args
}

func nativeSearchJoin(scope nativeScope, archived string, expressions []string) (string, []any) {
	ctes, args := nativeSearchHits(scope, archived, expressions)
	args = append(args, strings.Join(expressions, " OR "))
	return ` LEFT JOIN (` + ctes + `,previews AS MATERIALIZED (
 SELECT ranked.issue_id,ranked.search_score AS score,snippet(native_work_fts,-1,'[',']',' … ',32) AS snippet
 FROM ranked CROSS JOIN native_work_fts
 WHERE ranked.hit=1 AND native_work_fts.rowid=ranked.id/2 AND native_work_fts MATCH ?
 ) SELECT issue_id,score,substr(snippet,max(1,instr(snippet,'[')-60),240) AS snippet FROM previews
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
