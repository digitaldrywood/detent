-- +goose Up
DROP TRIGGER issues_capture_identity ON issues;
CREATE TRIGGER issues_capture_identity AFTER INSERT ON issues
    FOR EACH ROW EXECUTE FUNCTION capture_work_item_identity();

-- +goose Down
DROP TRIGGER issues_capture_identity ON issues;
CREATE TRIGGER issues_capture_identity BEFORE INSERT ON issues
    FOR EACH ROW EXECUTE FUNCTION capture_work_item_identity();
