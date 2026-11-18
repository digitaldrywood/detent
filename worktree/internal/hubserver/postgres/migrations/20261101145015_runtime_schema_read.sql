-- +goose Up
GRANT SELECT ON public.goose_db_version TO detent_runtime;

-- +goose Down
REVOKE SELECT ON public.goose_db_version FROM detent_runtime;
