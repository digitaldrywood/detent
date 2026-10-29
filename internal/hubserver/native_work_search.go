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
				phrases = append(phrases, `"`+scope+`_`+strings.TrimPrefix(expression, `"`))
			}
		}
		variants = append(variants, `scope : "`+scope+`" AND {title body} : (`+strings.Join(phrases, operator)+`)`)
	}
	return "(" + strings.Join(variants, " OR ") + ")"
}

const nativeSearchPostingBudget = 2000

func nativeSearchHits(scope nativeScope, archived string, expressions []string) (string, []any) {
	scopes := nativeSearchScopes(scope, archived)
	pending, args := nativeSearchIndexPending(scope)
	pending = "(" + pending + ") OR EXISTS(SELECT 1 FROM native_work_search_refresh WHERE organization_id=? AND project_id=?)"
	args = append(args, scope.organization, scope.project)
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
	values := make([]string, len(expressions))
	for i, expression := range expressions {
		values[i] = "(?,native_search_key(?))"
		args = append(args, expression, expression)
	}
	scopeValues := make([]string, len(scopes))
	for index, searchScope := range scopes {
		scopeValues[index] = "(?)"
		args = append(args, searchScope)
	}
	args = append(args, scope.organization, scope.project)
	args = append(args, visibilityArgs...)
	return `WITH index_state AS MATERIALIZED (SELECT ` + pending + ` AS incomplete), fallback_issues AS MATERIALIZED (
 SELECT i.id FROM index_state CROSS JOIN (
 SELECT i.* FROM issues i WHERE i.organization_id=? AND i.project_id=?` + archiveFilter + ` LIMIT ` + strconv.Itoa(nativeSearchPostingBudget) + `
 ) i WHERE index_state.incomplete AND (` + visibility + `)
 ), terms(matched,key) AS (VALUES ` + strings.Join(values, ",") + `), search_scopes(scope) AS (VALUES ` + strings.Join(scopeValues, ",") + `), fallback_candidates AS MATERIALIZED (
 SELECT d.id,d.issue_id,c.title,c.body FROM (
 SELECT document_id AS id FROM native_work_search_refresh WHERE organization_id=? AND project_id=? ORDER BY document_id DESC LIMIT ` + strconv.Itoa(nativeSearchPostingBudget) + `
 ) q CROSS JOIN native_work_documents d CROSS JOIN native_work_search_content c CROSS JOIN issues i WHERE d.id=q.id AND c.id=d.id AND i.id=d.issue_id` + archiveFilter + ` AND (` + visibility + `)
 UNION ALL
 SELECT d.id,d.issue_id,c.title,c.body
 FROM fallback_issues i CROSS JOIN native_work_documents d CROSS JOIN native_work_search_content c
 WHERE d.issue_id=i.id AND c.id=d.id AND NOT EXISTS(SELECT 1 FROM native_work_search_refresh WHERE document_id=d.id) LIMIT ` + strconv.Itoa(nativeSearchPostingBudget) + `
 ), fallback_documents AS MATERIALIZED (
 SELECT d.*,native_search_tokens(d.title,'')+native_search_tokens(d.body,'') AS tokens
 FROM fallback_candidates d WHERE EXISTS (SELECT 1 FROM native_work_search_refresh WHERE document_id=d.id) OR NOT EXISTS (
 SELECT 1 FROM search_scopes s CROSS JOIN native_work_search_lengths l WHERE l.document_id=d.id AND l.scope=s.scope
 )
 ), fallback_population AS MATERIALIZED (
 SELECT count(*) AS documents,avg(tokens) AS average FROM fallback_documents
 ), fallback_matches AS MATERIALIZED (
 SELECT d.id,d.issue_id,d.tokens,terms.matched,
 5*native_search_tokens(d.title,terms.matched)+native_search_tokens(d.body,terms.matched) AS frequency
 FROM fallback_documents d CROSS JOIN terms
 ), fallback_frequencies AS MATERIALIZED (
 SELECT matched,count(*) AS documents FROM fallback_matches WHERE frequency>0 GROUP BY matched
 ), fallback_scores AS MATERIALIZED (
 SELECT m.id,m.issue_id,count(*) AS terms,group_concat(m.matched,', ') AS matched,
 sum(native_search_bm25(m.frequency,m.tokens,p.average,p.documents,f.documents)) AS score
 FROM fallback_matches m CROSS JOIN fallback_population p JOIN fallback_frequencies f ON f.matched=m.matched
 WHERE m.frequency>0 GROUP BY m.id
 ), candidates AS MATERIALIZED (
 SELECT DISTINCT p.document_id,p.scope FROM terms CROSS JOIN search_scopes s
 CROSS JOIN native_work_search_postings p
 WHERE p.rowid IN (
 SELECT rowid FROM native_work_search_postings WHERE scope=s.scope AND term=terms.key
 ORDER BY impact DESC,document_id LIMIT ` + strconv.Itoa(nativeSearchPostingBudget) + `)
 ), indexed_matches AS MATERIALIZED (
 SELECT c.document_id,d.issue_id,c.scope,t.matched,
 CASE WHEN length(t.matched)>` + strconv.Itoa(nativeSearchPrefixLimit+3) + ` THEN
 (SELECT 5*native_search_tokens(title,t.matched)+native_search_tokens(body,t.matched) FROM native_work_search_content WHERE id=c.document_id)
 ELSE p.frequency END AS frequency,l.tokens,CAST(pop.tokens AS REAL)/pop.documents AS average,pop.documents,f.documents AS matching
 FROM candidates c CROSS JOIN terms t
 CROSS JOIN native_work_search_postings p CROSS JOIN native_work_search_lengths l
 CROSS JOIN native_work_search_population pop CROSS JOIN native_work_search_frequencies f
 CROSS JOIN native_work_documents d
 WHERE p.document_id=c.document_id AND p.scope=c.scope AND p.term=t.key
 AND l.document_id=c.document_id AND l.scope=c.scope AND pop.scope=c.scope
 AND f.scope=c.scope AND f.term=t.key AND d.id=c.document_id
 AND NOT EXISTS(SELECT 1 FROM native_work_search_refresh WHERE document_id=c.document_id)
 ), indexed_scores AS MATERIALIZED (
 SELECT document_id,issue_id,scope,count(*) AS terms,group_concat(matched,', ') AS matched,
 sum(native_search_bm25(frequency,tokens,average,documents,matching)) AS score
 FROM indexed_matches WHERE frequency>0 GROUP BY document_id,scope
 ), hits AS MATERIALIZED (
 SELECT issue_id,document_id*2 AS id,score,terms,matched FROM indexed_scores
 UNION ALL SELECT issue_id,id*2,score,terms,matched FROM fallback_scores
 ), ranked AS (
 SELECT hits.*,-terms+score/(1-score) AS search_score,
 row_number() OVER (PARTITION BY issue_id ORDER BY terms DESC,score,id) AS hit
 FROM hits
 ) `, args
}

func nativeSearchJoin(scope nativeScope, archived string, expressions []string, query string, workIncluded bool) (string, []any) {
	ctes, args := nativeSearchHits(scope, archived, expressions)
	args = append(args, strings.Join(expressions, " OR "), scope.organization, scope.project)
	visibility, visibilityArgs := scope.issueVisibilitySQL("i")
	args = append(args, visibilityArgs...)
	archiveFilter := " AND i.archived=0"
	if archived == "true" {
		archiveFilter = " AND i.archived=1"
	} else if archived == "all" {
		archiveFilter = ""
	}
	text := "i.title"
	if !workIncluded {
		text += " || ' ' || i.body"
	}
	args = append(args, query, query, query, query)
	number, project := nativeSearchNumber(query)
	args = append(args, scope.organization, scope.project, number, project, project, project)
	return `(` + ctes + `,previews AS MATERIALIZED (
 SELECT ranked.issue_id,ranked.search_score AS score,native_search_snippet(content.title,content.body,?) AS snippet
 FROM ranked CROSS JOIN native_work_search_content content
 WHERE ranked.hit=1 AND content.id=ranked.id/2
 ), legacy_issues AS MATERIALIZED (
 SELECT i.* FROM (SELECT i.* FROM issues i WHERE i.organization_id=? AND i.project_id=?` + archiveFilter + ` LIMIT ` + strconv.Itoa(nativeSearchPostingBudget) + `) i
 WHERE (` + visibility + `)
 ), selected AS (
 SELECT issue_id,score,substr(snippet,max(1,instr(snippet,'[')-60),240) AS snippet FROM previews
 UNION ALL SELECT i.id,0,NULL FROM legacy_issues i WHERE instr(lower(` + text + `),lower(?))>0
 OR instr(lower((SELECT name FROM projects WHERE id=i.project_id)||'#'||i.number),lower(?))>0
 OR instr(lower(i.project_id||'#'||i.number),lower(?))>0
 OR EXISTS(SELECT 1 FROM native_issue_page_filter_values WHERE issue_id=i.id AND kind='label' AND instr(lower(value),lower(?))>0)
 UNION ALL SELECT i.id,0,NULL FROM issues i WHERE i.organization_id=? AND i.project_id=? AND i.number=?
 AND (?='' OR lower(i.project_id)=lower(?) OR lower((SELECT name FROM projects WHERE id=i.project_id))=lower(?))
 ) SELECT issue_id,min(score) AS score,max(snippet) AS snippet FROM selected GROUP BY issue_id
 ) search`, args
}

const nativeLegacySearchText = `i.title || ' ' || i.body || ' ' || i.labels_json || ' ' ||
 (SELECT name FROM projects WHERE id = i.project_id) || '#' || i.number || ' ' || i.project_id || '#' || i.number`

const nativeNumberSearchJoin = ` LEFT JOIN (SELECT ? AS number, ? AS project, ? AS score) number_search
 ON i.number = number_search.number AND (number_search.project = '' OR lower(number_search.project) = lower(i.project_id)
 OR lower(number_search.project) = lower((SELECT name FROM projects WHERE id = i.project_id))) `

const nativeSearchScore = `COALESCE(number_search.score, search.score, 0)`

const nativeSearchProjection = `, ` + nativeSearchScore + ` AS search_score, COALESCE(search.snippet,
 substr(` + nativeLegacySearchText + `, max(1, instr(lower(` + nativeLegacySearchText + `), lower(?)) - 60), 240)) AS search_snippet`
