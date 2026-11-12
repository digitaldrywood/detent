package hubserver

const nativeTerminalEnteredAt = `(SELECT e.recorded_at FROM native_transition_facts e
JOIN workflow_states target ON target.project_id = e.project_id AND target.detent_state = e.to_state AND target.terminal = 1
JOIN workflow_states origin ON origin.project_id = e.project_id AND origin.detent_state = e.from_state AND origin.terminal = 0
WHERE e.organization_id = i.organization_id AND e.project_id = i.project_id AND e.work_item_id = i.native_id
ORDER BY e.sequence DESC LIMIT 1)`
