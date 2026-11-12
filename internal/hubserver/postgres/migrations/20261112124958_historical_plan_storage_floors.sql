-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE POLICY historical_storage_floor_plans ON hosted_plans FOR SELECT TO detent_migration USING ((id,version) IN (('starter',1),('team',2),('growth',2)));
CREATE POLICY historical_storage_floor_allowances ON hosted_plan_allowances TO detent_migration USING (metric='collaboration_bytes' AND (plan_id,plan_version) IN (('starter',1),('team',2),('growth',2))) WITH CHECK (metric='collaboration_bytes' AND (plan_id,plan_version) IN (('starter',1),('team',2),('growth',2)));
INSERT INTO hosted_plan_allowances(organization_id,plan_id,plan_version,metric,amount)
SELECT p.organization_id,p.id,p.version,'collaboration_bytes',floor.amount
FROM hosted_plans p
JOIN (VALUES ('starter',1,26843545600::bigint),('team',2,107374182400::bigint),('growth',2,268435456000::bigint)) AS floor(id,version,amount)
 ON p.id=floor.id AND p.version=floor.version
ON CONFLICT(organization_id,plan_id,plan_version,metric) DO UPDATE
 SET amount=GREATEST(hosted_plan_allowances.amount,EXCLUDED.amount)
 WHERE hosted_plan_allowances.amount<EXCLUDED.amount;
DROP POLICY historical_storage_floor_allowances ON hosted_plan_allowances;
DROP POLICY historical_storage_floor_plans ON hosted_plans;

-- +goose Down
SELECT 1;
