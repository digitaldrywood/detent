package hubserver

import "github.com/digitaldrywood/detent/internal/hubserver/hubapi"

const nativeSearchTermLimit = hubapi.SearchTermLimit

const nativeLegacySearchText = `i.title || ' ' || i.body || ' ' || i.labels_json || ' ' ||
 (SELECT name FROM projects WHERE id = i.project_id) || '#' || i.number || ' ' || i.project_id || '#' || i.number`

const nativeNumberSearchJoin = ` LEFT JOIN (SELECT ? AS number, ? AS project, ? AS score) number_search
 ON i.number = number_search.number AND (number_search.project = '' OR lower(number_search.project) = lower(i.project_id)
 OR lower(number_search.project) = lower((SELECT name FROM projects WHERE id = i.project_id))) `

const nativeSearchScore = `COALESCE(number_search.score, search.score, 0)`

const nativeSearchProjection = `, ` + nativeSearchScore + ` AS search_score, COALESCE(search.snippet,
 substr(` + nativeLegacySearchText + `, max(1, instr(lower(` + nativeLegacySearchText + `), lower(?)) - 60), 240)) AS search_snippet`
