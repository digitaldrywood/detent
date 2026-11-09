-- +goose Up
SET LOCAL lock_timeout = '5s';
DROP TRIGGER local_board_issue_population ON issues;
CREATE TRIGGER local_board_issue_population AFTER INSERT OR DELETE ON issues
FOR EACH ROW EXECUTE FUNCTION local_board_source_population();
CREATE TRIGGER local_board_issue_population_update AFTER UPDATE OF archived,workflow_state_name ON issues
FOR EACH ROW WHEN ((OLD.organization_id,OLD.project_id,OLD.archived,OLD.workflow_state_name)
 IS DISTINCT FROM (NEW.organization_id,NEW.project_id,NEW.archived,NEW.workflow_state_name))
EXECUTE FUNCTION local_board_source_population();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER local_board_issue_population_update ON issues;
DROP TRIGGER local_board_issue_population ON issues;
CREATE TRIGGER local_board_issue_population AFTER INSERT OR DELETE OR UPDATE OF archived,workflow_state_name ON issues
FOR EACH ROW EXECUTE FUNCTION local_board_source_population();
