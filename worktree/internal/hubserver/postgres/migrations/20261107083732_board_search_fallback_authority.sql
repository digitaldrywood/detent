-- +goose Up
GRANT SELECT(organization_id,project_id,work_item_id,page_title) ON native_board_items TO detent_search_reader_v2;
GRANT SELECT(organization_id,project_id,work_item_id,body) ON native_work_item_pages TO detent_search_reader_v2;
GRANT SELECT(organization_id,project_id,native_id,actor_principal_id) ON issues TO detent_search_reader_v2;
CREATE POLICY board_search_reader ON native_board_items FOR SELECT TO detent_search_reader_v2 USING(true);
CREATE POLICY board_search_reader ON native_work_item_pages FOR SELECT TO detent_search_reader_v2 USING(true);
CREATE POLICY board_search_reader ON issues FOR SELECT TO detent_search_reader_v2 USING(true);

-- +goose Down
DROP POLICY board_search_reader ON issues;
DROP POLICY board_search_reader ON native_work_item_pages;
DROP POLICY board_search_reader ON native_board_items;
REVOKE SELECT(organization_id,project_id,work_item_id,body) ON native_work_item_pages FROM detent_search_reader_v2;
REVOKE SELECT(organization_id,project_id,native_id,actor_principal_id) ON issues FROM detent_search_reader_v2;
REVOKE SELECT(organization_id,project_id,work_item_id,page_title) ON native_board_items FROM detent_search_reader_v2;
