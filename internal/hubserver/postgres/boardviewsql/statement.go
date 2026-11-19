package boardviewsql

import (
	"strconv"
	"strings"
)

const (
	dirtyOverlayLimit    = 256
	arrayRowElementLimit = 256
)

type boardViewColumn struct {
	name, kind string
}

var boardViewCardColumns = []boardViewColumn{
	{"number", "bigint"}, {"revision", "bigint"}, {"state", "text"}, {"archived", "boolean"}, {"terminal", "boolean"}, {"running", "boolean"},
	{"dispatchable", "boolean"}, {"priority", "bigint"}, {"activity_at", "timestamptz"}, {"identifier", "text"}, {"closed_at", "timestamptz"},
	{"created_at", "timestamptz"}, {"updated_at", "timestamptz"}, {"last_activity_at", "timestamptz"}, {"actor_principal_id", "text"}, {"issue_type", "text"},
	{"title", "text"}, {"labels", "text[]"}, {"assignees", "text[]"}, {"has_links", "boolean"}, {"provenance_provider", "text"}, {"provenance_external_id", "text"},
	{"provenance_created_at", "timestamptz"}, {"provenance_updated_at", "timestamptz"}, {"provenance_observed_at", "timestamptz"}, {"pull_request_number", "bigint"},
	{"pull_request_url", "text"}, {"list_change_branch", "text"}, {"list_change_head_sha", "text"}, {"card_ready", "boolean"}, {"status_line", "text"},
	{"attempt_id", "text"}, {"attempt_count", "bigint"}, {"attempt_status", "text"}, {"attempt_runner_id", "text"}, {"attempt_machine_id", "text"},
	{"attempt_started_at", "timestamptz"}, {"attempt_backend", "text"}, {"attempt_model", "text"}, {"attempt_effort", "text"}, {"execution_id", "text"},
	{"execution_running", "boolean"}, {"execution_stage", "text"}, {"execution_runner_id", "text"}, {"execution_started_at", "timestamptz"},
	{"execution_backend", "text"}, {"execution_model", "text"}, {"execution_effort", "text"}, {"change_id", "text"}, {"change_title", "text"},
	{"change_status", "text"}, {"change_external_provider", "text"}, {"change_external_id", "text"}, {"change_external_url", "text"},
	{"change_principal_id", "text"}, {"change_version_id", "text"}, {"change_policy_id", "text"}, {"change_review_policy_id", "text"}, {"change_base_status", "text"},
	{"actor_kind", "text"}, {"actor_via_key", "text"}, {"actor_display_name", "text"}, {"actor_agent", "text"}, {"actor_public_name", "text"}, {"web_url", "text"},
	{"omitted_fields", "text[]"}, {"reporter_status", "text"}, {"reporter_view_status", "text"}, {"scheduling_eligible", "boolean"}, {"provenance_author_id", "text"},
	{"provenance_author_display_name", "text"}, {"verification_present", "boolean"}, {"verification_state", "text"}, {"verified_by_present", "boolean"},
	{"verified_by_kind", "text"}, {"verified_by_principal_id", "text"}, {"verified_by_via_key", "text"}, {"verified_by_display_name", "text"}, {"verified_by_agent", "text"},
	{"verified_at", "timestamptz"}, {"report_context_present", "boolean"}, {"report_context_url", "text"}, {"report_context_app_build", "text"},
	{"report_context_browser", "text"}, {"report_context_os", "text"}, {"report_context_viewport", "text"}, {"report_context_captured_at", "text"},
	{"contract_present", "boolean"}, {"contract_human_action", "text"}, {"attempt_expires_at", "timestamptz"}, {"attempt_role", "text"},
	{"execution_expires_at", "timestamptz"}, {"execution_role", "text"},
}

func PublicActorName(organization, principal string) string {
	return `coalesce((SELECT nullif(member.email,'') FROM hosted_members member WHERE member.organization_id=` + organization + ` AND member.principal_id=` + principal + ` LIMIT 1),(SELECT credential.name FROM api_tokens credential WHERE credential.organization_id=` + organization + ` AND credential.id=` + principal + `),'')`
}

var boardViewGenericColumns = []boardViewColumn{
	{"t1", "text"}, {"t2", "text"}, {"t3", "text"}, {"t4", "text"}, {"t5", "text"}, {"t6", "text"}, {"t7", "text"}, {"t8", "text"},
	{"i1", "bigint"}, {"i2", "bigint"}, {"i3", "bigint"}, {"i4", "bigint"}, {"i5", "bigint"}, {"i6", "bigint"}, {"i7", "bigint"}, {"i8", "bigint"},
	{"b1", "boolean"}, {"b2", "boolean"}, {"b3", "boolean"}, {"b4", "boolean"}, {"b5", "boolean"}, {"b6", "boolean"}, {"b7", "boolean"}, {"b8", "boolean"},
	{"d1", "timestamptz"}, {"d2", "timestamptz"},
}

var boardViewArrayColumns = []boardViewColumn{
	{"ta1", "text[]"}, {"ta2", "text[]"}, {"ta3", "text[]"}, {"ta4", "text[]"}, {"ta5", "text[]"}, {"ta6", "text[]"}, {"ta7", "text[]"}, {"ta8", "text[]"},
	{"ia1", "bigint[]"}, {"ia2", "bigint[]"}, {"ba1", "boolean[]"}, {"ba2", "boolean[]"}, {"ba3", "boolean[]"}, {"da1", "timestamptz[]"}, {"da2", "timestamptz[]"},
}

func boardViewRow(kind, project, id string, values map[string]string) string {
	parts := []string{"'" + kind + "'::text", "(" + project + ")::text", "(" + id + ")::text"}
	for _, group := range [][]boardViewColumn{boardViewCardColumns, boardViewGenericColumns, boardViewArrayColumns} {
		for _, column := range group {
			value, found := values[column.name]
			if !found {
				value = "NULL"
			}
			parts = append(parts, "("+value+")::"+column.kind)
		}
	}
	return "SELECT " + strings.Join(parts, ",")
}

func boardViewCardRow(kind, alias string) string {
	values := map[string]string{}
	for _, column := range boardViewCardColumns {
		values[column.name] = alias + "." + column.name
	}
	values["actor_public_name"] = PublicActorName("$1", alias+".actor_principal_id")
	return boardViewRow(kind, alias+".project_id", alias+".work_item_id", values)
}

func boardViewSource(reporter bool) string {
	if reporter {
		return "page_reporter_"
	}
	return "page_"
}

func boardViewArrayRow(kind, source string, slots map[string]string) string {
	values := map[string]string{}
	for slot, field := range slots {
		values[slot] = "array_agg(a." + field + ")"
	}
	return boardViewRow(kind, "NULL", "NULL", values) + " FROM (SELECT s.*,(row_number() OVER ()-1)/" + strconv.Itoa(arrayRowElementLimit) + " AS batch FROM (" + source + ") s) a GROUP BY a.batch"
}

const boardViewLinked = ` FROM linked c JOIN native_board_items x ON x.organization_id=$1 AND x.project_id=c.project_id AND x.work_item_id=c.work_item_id`

const boardViewReadable = `lp.id IS NOT NULL AND lp.id=ANY($3::text[]) AND ($4::text='' OR li.actor_principal_id=$4)`

func boardViewLinkRows(reporter bool) string {
	source := boardViewSource(reporter)
	names := []string{"project_id", "id", "title", "state", "identifier", "web_url", "number", "terminal", "dispatchable", "closed_at", "started_at"}
	var relations []string
	for _, relation := range [][2]string{{"blocker", "blockers"}, {"dependent", "dependents"}, {"related", "related"}} {
		arrays := make([]string, len(names))
		for i, name := range names {
			arrays[i] = "x." + source + relation[1] + "_" + name
		}
		relations = append(relations, "SELECT '"+relation[0]+"'::text AS relation,u.* FROM unnest("+strings.Join(arrays, ",")+") WITH ORDINALITY u("+strings.Join(names, ",")+",n)")
	}
	links := "SELECT l.relation,l.project_id::text AS project,c.work_item_id::text AS card,l.id::text AS id,l.title::text AS title," +
		"l.state::text AS state,l.identifier::text AS identifier,l.web_url::text AS web_url,l.number::bigint AS number,l.n::bigint AS n," +
		"l.terminal::boolean AS terminal,l.dispatchable::boolean AS dispatchable,l.closed_at::timestamptz AS closed_at,l.started_at::timestamptz AS started_at" + boardViewLinked + `
CROSS JOIN LATERAL (` + strings.Join(relations, "\nUNION ALL ") + `) l
LEFT JOIN issues li ON li.organization_id=$1 AND li.project_id=l.project_id AND li.native_id=l.id
LEFT JOIN projects lp ON lp.organization_id=$1 AND lp.id=li.project_id AND lp.deleted_at IS NULL AND lp.profile='native'
WHERE ` + boardViewReadable
	return boardViewArrayRow("links", links, map[string]string{"ta1": "relation", "ta2": "project", "ta3": "card", "ta4": "id", "ta5": "title", "ta6": "state",
		"ta7": "identifier", "ta8": "web_url", "ia1": "number", "ia2": "n", "ba1": "terminal", "ba2": "dispatchable", "da1": "closed_at", "da2": "started_at"})
}

func boardViewDependencyRows(reporter bool) string {
	dependencies := "SELECT c.project_id::text AS project,c.work_item_id::text AS card,l.id::text AS id,l.n::bigint AS n" + boardViewLinked + `
CROSS JOIN LATERAL unnest(x.` + boardViewSource(reporter) + `dependencies) WITH ORDINALITY l(id,n)
LEFT JOIN issues li ON li.organization_id=$1 AND li.native_id=l.id
LEFT JOIN projects lp ON lp.organization_id=$1 AND lp.id=li.project_id AND lp.deleted_at IS NULL AND lp.profile='native'
WHERE li.id IS NOT NULL AND ` + boardViewReadable
	return boardViewArrayRow("dependencies", dependencies, map[string]string{"ta1": "project", "ta2": "card", "ta3": "id", "ia1": "n"})
}

func boardViewReasonRows(reporter bool) string {
	field := func(name string) string { return "x." + boardViewSource(reporter) + name }
	reasons := "SELECT c.project_id::text AS project,c.work_item_id::text AS card," + field("blocked_reason_text") + "::text AS t1," + field("blocked_reason_basis") + "::text AS t2," +
		field("blocked_reason_current_basis") + "::text AS t3," + field("unblocks_reason_text") + "::text AS t4," + field("unblocks_reason_basis") + "::text AS t5," + field("unblocks_reason_current_basis") + "::text AS t6," +
		field("blocked_reason_revision") + "::bigint AS i1," + field("unblocks_reason_revision") + "::bigint AS i2," + field("dependency_ready") + "::boolean AS b1" + boardViewLinked
	return boardViewArrayRow("reasons", reasons, map[string]string{"ta1": "project", "ta2": "card", "ta3": "t1", "ta4": "t2", "ta5": "t3", "ta6": "t4", "ta7": "t5", "ta8": "t6",
		"ia1": "i1", "ia2": "i2", "ba1": "b1"})
}

func boardViewStateRows() string {
	return boardViewArrayRow("states", `SELECT w.project_id::text AS project,w.name::text AS name,t.target_name::text AS target,w.board_rank::bigint AS rank,t.transition_rank::bigint AS transition,
 w.terminal::boolean AS terminal,w.dispatchable::boolean AS dispatchable,w.operator_only::boolean AS operator_only FROM visible v JOIN project_workflow_states w ON w.organization_id=$1 AND w.project_id=v.id
 LEFT JOIN project_workflow_transitions t ON t.organization_id=w.organization_id AND t.project_id=w.project_id AND t.source_name=w.name`,
		map[string]string{"ta1": "project", "ta2": "name", "ta3": "target", "ia1": "rank", "ia2": "transition", "ba1": "terminal", "ba2": "dispatchable", "ba3": "operator_only"})
}

func CompletionCountsSQL(project, audience, excludeWorkspace, at string) string {
	clock := "coalesce(" + at + "::timestamptz,statement_timestamp())"
	return `SELECT CASE WHEN GROUPING(b.state)=1 THEN 'total' ELSE 'lane' END AS kind,coalesce(b.state,'') AS state,
 CASE WHEN GROUPING(b.archived)=1 THEN 'all' ELSE b.archived::text END AS archive_scope,
 count(*) FILTER (WHERE b.state='Done' AND b.closed_at >= ` + clock + `-interval '1 hour') AS shipped_1h,
 count(*) FILTER (WHERE b.state='Done' AND b.closed_at >= ` + clock + `-interval '24 hours') AS shipped_24h,
 count(*) FILTER (WHERE b.state='Done') AS shipped_7d,
 count(*) FILTER (WHERE b.state='Cancelled' AND b.closed_at >= ` + clock + `-interval '1 hour') AS cancelled_1h,
 count(*) FILTER (WHERE b.state='Cancelled' AND b.closed_at >= ` + clock + `-interval '24 hours') AS cancelled_24h,
 count(*) FILTER (WHERE b.state='Cancelled') AS cancelled_7d
 FROM native_board_items b
 WHERE b.organization_id=$1 AND b.project_id=` + project + ` AND b.page_ready AND b.terminal AND b.state IN ('Done','Cancelled')
 AND coalesce(b.closed_at,'-infinity'::timestamptz)>=` + clock + `-interval '168 hours'
 AND coalesce(b.closed_at,'-infinity'::timestamptz)<=` + clock + `
 AND (NOT ` + excludeWorkspace + `::boolean OR NOT 'detent:workspace'=ANY(coalesce(b.page_labels,'{}')))
 AND (` + audience + `::text='' OR b.actor_principal_id=` + audience + ` AND EXISTS(SELECT 1 FROM issues i
  WHERE i.organization_id=$1 AND i.project_id=b.project_id AND i.native_id=b.work_item_id AND i.actor_principal_id=` + audience + ` OFFSET 0))
 GROUP BY GROUPING SETS ((),(b.state),(b.archived),(b.archived,b.state))`
}

func boardViewSQL(reporter bool) string {
	revision := func(subject, id string) string {
		return "coalesce((SELECT r.revision FROM native_board_access_revisions r WHERE r.organization_id=$1 AND r.subject_kind='" + subject + "' AND r.subject_id=" + id + "),0)"
	}
	after := "(coalesce(c.closed_at,'-infinity'::timestamptz),c.identifier)<(coalesce($13::timestamptz,'-infinity'::timestamptz),$14::text)"
	scoped := func(archived string) string {
		return "c.ready AND NOT c.workspace AND " + archived + "=$5 AND ($4::text='' OR c.actor_principal_id=$4 AND EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=$1 AND i.project_id=c.project_id AND i.native_id=c.work_item_id AND i.actor_principal_id=$4))"
	}
	placed := func(state, terminal string) string {
		return "(CASE WHEN $11::boolean THEN c.work_item_id=ANY($10::text[]) ELSE NOT " + terminal + " AND (c.project_id," + state + ") IN (SELECT project_id,name FROM lanes WHERE NOT terminal) AND (NOT $15::boolean OR " + after + ") END)"
	}
	exact, live := []string{"c.project_id", "c.work_item_id"}, []string{"c.project_id", "c.work_item_id"}
	overlay := map[string]string{"state": "coalesce(d.state,c.state)", "terminal": "coalesce(d.terminal,c.terminal)", "dispatchable": "coalesce(d.dispatchable,c.dispatchable)", "archived": "coalesce(d.archived,c.archived)"}
	for _, column := range boardViewCardColumns {
		if column.name == "actor_public_name" {
			continue
		}
		exact = append(exact, "c."+column.name)
		if value, ok := overlay[column.name]; ok {
			live = append(live, value+" AS "+column.name)
		} else {
			live = append(live, "c."+column.name)
		}
	}
	affected := " WHERE NOT $11::boolean OR v.id IN (SELECT project_id FROM cards)"
	ctes := `WITH visible AS MATERIALIZED (SELECT p.id FROM projects p WHERE p.organization_id=$1 AND p.id=ANY($2::text[]) AND p.deleted_at IS NULL AND p.profile='native'),
lanes AS MATERIALIZED (SELECT w.project_id,w.name,w.terminal FROM project_workflow_states w JOIN visible v ON w.organization_id=$1 AND w.project_id=v.id
 WHERE NOT $11::boolean AND ($12::text='all' OR NOT w.terminal AND ($12::text='open' OR w.dispatchable)) AND (cardinality($16::text[])=0 OR w.name=ANY($16::text[])) AND w.name<>ALL($17::text[])),
dirty AS MATERIALIZED (SELECT d.project_id,d.work_item_id,i.workflow_state_name AS state,i.archived,w.terminal,w.dispatchable FROM visible v
 CROSS JOIN LATERAL (SELECT d.project_id,d.work_item_id FROM native_board_dirty d WHERE d.organization_id=$1 AND d.project_id=v.id ORDER BY d.work_item_id LIMIT ` + strconv.Itoa(dirtyOverlayLimit) + `) d
 JOIN issues i ON i.organization_id=$1 AND i.project_id=d.project_id AND i.native_id=d.work_item_id
 LEFT JOIN project_workflow_states w ON w.organization_id=$1 AND w.project_id=d.project_id AND w.name=i.workflow_state_name),
cards AS MATERIALIZED (SELECT ` + strings.Join(exact, ",") + ` FROM native_board_cards c WHERE c.organization_id=$1 AND c.project_id IN (SELECT id FROM visible) AND ` + scoped("c.archived") + `
 AND NOT EXISTS(SELECT 1 FROM dirty d WHERE d.project_id=c.project_id AND d.work_item_id=c.work_item_id) AND ` + placed("c.state", "c.terminal") + `
 UNION ALL SELECT ` + strings.Join(live, ",") + ` FROM dirty d JOIN native_board_cards c ON c.organization_id=$1 AND c.project_id=d.project_id AND c.work_item_id=d.work_item_id
 WHERE ` + scoped("coalesce(d.archived,c.archived)") + ` AND ` + placed("coalesce(d.state,c.state)", "coalesce(d.terminal,c.terminal)") + `),
closed AS MATERIALIZED (SELECT x.* FROM (SELECT project_id,name FROM lanes WHERE terminal) t
 CROSS JOIN LATERAL (SELECT c.* FROM native_board_cards c WHERE c.organization_id=$1 AND c.project_id=t.project_id AND c.state=t.name AND c.terminal AND ` + scoped("c.archived") + `
  AND NOT EXISTS(SELECT 1 FROM dirty d WHERE d.project_id=c.project_id AND d.work_item_id=c.work_item_id AND (coalesce(d.state,c.state)<>c.state OR coalesce(d.archived,c.archived)<>c.archived))
  AND (NOT $15::boolean OR ` + after + `)
  ORDER BY coalesce(c.closed_at,'-infinity'::timestamptz) DESC,c.identifier DESC LIMIT $9) x),
completion AS MATERIALIZED (SELECT v.id AS project_id,w.* FROM visible v
 CROSS JOIN LATERAL (` + CompletionCountsSQL("v.id", "$4", "true", "$18") + `) w` + affected + `),
linked AS MATERIALIZED (SELECT project_id,work_item_id FROM cards WHERE has_links UNION SELECT project_id,work_item_id FROM closed WHERE has_links),
evaluated AS MATERIALIZED (SELECT project_id,change_policy_id,change_review_policy_id FROM cards WHERE change_id<>'' AND change_version_id<>'' AND change_base_status<>'landed'
 UNION SELECT project_id,change_policy_id,change_review_policy_id FROM closed WHERE change_id<>'' AND change_version_id<>'' AND change_base_status<>'landed')
`
	readiness := `(EXISTS(SELECT 1 FROM native_board_dirty d WHERE d.organization_id=$1 AND d.project_id=v.id OFFSET ` + strconv.Itoa(dirtyOverlayLimit) + `)
  OR EXISTS(SELECT 1 FROM (SELECT d.work_item_id FROM native_board_dirty d WHERE d.organization_id=$1 AND d.project_id=v.id LIMIT ` + strconv.Itoa(dirtyOverlayLimit) + `) d
   WHERE NOT EXISTS(SELECT 1 FROM native_board_items b JOIN issues i ON i.organization_id=b.organization_id AND i.project_id=b.project_id AND i.native_id=b.work_item_id
    WHERE b.organization_id=$1 AND b.project_id=v.id AND b.work_item_id=d.work_item_id AND b.actor_principal_id=coalesce(i.actor_principal_id,'') OFFSET 0))
  OR EXISTS(SELECT 1 FROM native_board_items b WHERE b.organization_id=$1 AND b.project_id=v.id AND NOT b.page_ready)
  OR EXISTS(SELECT 1 FROM native_board_items b WHERE b.organization_id=$1 AND b.project_id=v.id AND NOT b.list_ready))`
	rows := []string{
		boardViewRow("access", "NULL", "NULL", map[string]string{"i1": revision("organization", "$1"), "i2": revision("token", "$6"), "i3": revision("token", "$7"), "i4": revision("member", "$8")}),
		boardViewRow("project", "p.id", "p.id", map[string]string{"t1": "p.name", "t2": "p.slug", "t3": "p.profile", "b1": "p.require_dependencies"}) + ` FROM visible v JOIN projects p ON p.organization_id=$1 AND p.id=v.id`,
		boardViewStateRows(),
		boardViewRow("sequence", "v.id", "NULL", map[string]string{"i1": "coalesce((SELECT s.sequence FROM native_board_sequences s WHERE s.organization_id=$1 AND s.project_id=v.id),0)", "b1": readiness,
			"d1": "(SELECT d.queued_at FROM native_board_dirty d WHERE d.organization_id=$1 AND d.project_id=v.id ORDER BY d.queued_at LIMIT 1)"}) + ` FROM visible v` + affected,
		boardViewRow("count", "o.project_id", "o.state", map[string]string{"t1": "o.kind", "t2": "'scope'", "i1": "o.total", "i2": "o.running", "i3": "coalesce(w.shipped_1h,0)", "i4": "coalesce(w.shipped_24h,0)",
			"i5": "coalesce(w.shipped_7d,0)", "i6": "coalesce(w.cancelled_1h,0)", "i7": "coalesce(w.cancelled_24h,0)", "i8": "coalesce(w.cancelled_7d,0)", "d1": "o.projected_at"}) +
			` FROM visible v JOIN native_board_page_summaries o ON o.organization_id=$1 AND o.project_id=v.id AND o.audience=$4 AND o.archive_scope=$5::text AND o.exclude_workspace
 LEFT JOIN completion w ON w.project_id=o.project_id AND w.kind=CASE WHEN o.kind='closed' THEN 'total' ELSE o.kind END AND w.state=o.state AND w.archive_scope=o.archive_scope` + affected,
		boardViewRow("count", "o.project_id", "o.state", map[string]string{"t1": "o.kind", "t2": "'all'", "i1": "o.total", "i2": "o.running", "i3": "coalesce(w.shipped_1h,0)", "i4": "coalesce(w.shipped_24h,0)",
			"i5": "coalesce(w.shipped_7d,0)", "i6": "coalesce(w.cancelled_1h,0)", "i7": "coalesce(w.cancelled_24h,0)", "i8": "coalesce(w.cancelled_7d,0)", "d1": "o.projected_at"}) +
			` FROM visible v JOIN native_board_page_summaries o ON o.organization_id=$1 AND o.project_id=v.id AND o.audience=$4 AND o.archive_scope='all' AND o.exclude_workspace AND o.kind IN ('total','lane') AND $5::text<>'all'
 LEFT JOIN completion w ON w.project_id=o.project_id AND w.kind=CASE WHEN o.kind='closed' THEN 'total' ELSE o.kind END AND w.state=o.state AND w.archive_scope=o.archive_scope` + affected,
		boardViewCardRow("card", "c") + ` FROM cards c`,
		boardViewCardRow("closed", "c") + ` FROM closed c`,
		boardViewLinkRows(reporter), boardViewDependencyRows(reporter), boardViewReasonRows(reporter),
		boardViewRow("change", "c.project_id", "c.id", nil) + ` FROM change_requests c JOIN issues i ON i.organization_id=c.organization_id AND i.project_id=c.project_id AND i.native_id=c.work_item_id
WHERE c.organization_id=$1 AND (c.project_id,c.id) IN (SELECT project_id,change_id FROM cards WHERE change_id<>'' UNION SELECT project_id,change_id FROM closed WHERE change_id<>'') AND ($4::text='' OR i.actor_principal_id=$4)`,
		boardViewRow("policy", "p.project_id", "r.policy_id", map[string]string{"t1": "COALESCE(a.approved_by,r.approved_by)", "d1": "COALESCE(a.approved_at,r.approved_at)"}) +
			` FROM project_policies p JOIN policy_revisions r ON r.organization_id=p.organization_id AND r.project_id=p.project_id AND r.policy_id=p.policy_id
LEFT JOIN policy_revision_approvals a ON a.organization_id=r.organization_id AND a.project_id=r.project_id AND a.policy_id=r.policy_id
WHERE p.organization_id=$1 AND p.project_id IN (SELECT project_id FROM evaluated)`,
		boardViewRow("review", "c.project_id", "r.review_policy_id", map[string]string{"t1": "r.policy_id", "t2": "r.updated_by", "i1": "c.revision", "d1": "r.updated_at"}) +
			` FROM change_review_policies c JOIN change_review_policy_revisions r ON r.organization_id=c.organization_id AND r.project_id=c.project_id AND r.review_policy_id=c.review_policy_id
WHERE c.organization_id=$1 AND c.project_id IN (SELECT project_id FROM evaluated)`,
		boardViewRow("policy_facts", "c.project_id", "c.policy_id", map[string]string{"t1": "c.payload_sha256", "t2": "c.gate_landing_mode", "t3": "c.gate_kind", "t4": "c.gate_landing_command_digest", "t5": "c.gate_github_ci_digest",
			"t6": "c.gate_plan_review", "t7": "c.gate_automated_review", "i1": "c.gate_required_checks", "b1": "c.gate_github_pull_request", "b2": "c.gate_validator", "b3": "c.gate_security_audit", "b4": "c.gate_human_review", "b5": "c.gate_plan_enabled"}) +
			` FROM policy_revision_compatibility c WHERE c.organization_id=$1 AND c.project_id IN (SELECT project_id FROM evaluated)
AND (c.policy_id IN (SELECT e.change_policy_id FROM evaluated e WHERE e.project_id=c.project_id) OR c.policy_id IN (SELECT p.policy_id FROM project_policies p WHERE p.organization_id=c.organization_id AND p.project_id=c.project_id))`,
		boardViewRow("review_facts", "f.project_id", "f.review_policy_id", map[string]string{"t1": "f.policy_id", "t2": "f.payload_sha256", "i1": "f.required_check_count", "b1": "f.require_review"}) +
			` FROM review_policy_revision_compatibility f WHERE f.organization_id=$1 AND f.project_id IN (SELECT project_id FROM evaluated)
AND (f.review_policy_id IN (SELECT e.change_review_policy_id FROM evaluated e WHERE e.project_id=f.project_id) OR f.review_policy_id IN (SELECT c.review_policy_id FROM change_review_policies c WHERE c.organization_id=f.organization_id AND c.project_id=f.project_id))`,
		boardViewRow("review_check", "k.project_id", "k.review_policy_id", map[string]string{"t1": "k.name", "t2": "k.principal_id", "t3": "k.workflow_id", "t4": "k.workflow_sha256", "t5": "k.source", "i1": "k.ordinal", "i2": "k.max_age_seconds"}) +
			` FROM review_policy_revision_checks k WHERE k.organization_id=$1 AND k.project_id IN (SELECT project_id FROM evaluated)
AND (k.review_policy_id IN (SELECT e.change_review_policy_id FROM evaluated e WHERE e.project_id=k.project_id) OR k.review_policy_id IN (SELECT c.review_policy_id FROM change_review_policies c WHERE c.organization_id=k.organization_id AND c.project_id=k.project_id))`,
	}
	return ctes + strings.Join(rows, "\nUNION ALL\n")
}

var statements = [2]string{boardViewSQL(false), boardViewSQL(true)}

func Statement(reporter bool) string {
	if reporter {
		return statements[1]
	}
	return statements[0]
}
