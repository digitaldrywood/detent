-- +goose Up
CREATE AGGREGATE request_histogram_sum(bigint[]) (
 SFUNC = request_histogram_add,
 STYPE = bigint[],
 INITCOND = '{0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0}'
);

-- +goose Down
DROP AGGREGATE request_histogram_sum(bigint[]);
