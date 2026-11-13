package hubserver

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

// resolveConversationWorkItems fills the linked issue summary of every linked
// record in one query. A record whose issue is no longer readable keeps a nil
// summary and projects work_item: null rather than failing the read.
func resolveConversationWorkItems(ctx context.Context, query hubapi.NativeQueryer, records []*conversationRecord) error {
	linked := make([]string, 0, len(records))
	for _, record := range records {
		if record.WorkItemID != "" {
			linked = append(linked, record.WorkItemID)
		}
	}
	if len(linked) == 0 {
		return nil
	}
	ids, err := hubapi.SQLList(linked)
	if err != nil {
		return fmt.Errorf("encode linked work items: %w", err)
	}
	rows, err := hubapi.QuerySQL(ctx, query, `SELECT i.organization_id, i.project_id, i.native_id, p.name, COALESCE(i.number, 0), i.title, COALESCE(ws.detent_state, '')
FROM issues i JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.native_id IN (SELECT value FROM sql_rows(?))`, ids)
	if err != nil {
		return fmt.Errorf("read linked work items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := map[string]conversationWorkItem{}
	for rows.Next() {
		var organization, project, id, name, title, lane string
		var number int64
		if err := rows.Scan(&organization, &project, &id, &name, &number, &title, &lane); err != nil {
			return fmt.Errorf("read linked work items: %w", err)
		}
		items[conversationWorkItemKey(organization, project, id)] = conversationWorkItem{
			ID: id, Identifier: fmt.Sprintf("%s#%d", name, number), Title: title, Lane: lane,
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read linked work items: %w", err)
	}
	for _, record := range records {
		item, ok := items[conversationWorkItemKey(string(record.OrganizationID), string(record.ProjectID), record.WorkItemID)]
		if !ok {
			continue
		}
		record.WorkItem = &item
	}
	return nil
}

func conversationWorkItemKey(organization, project, workItem string) string {
	return organization + "/" + project + "/" + workItem
}
