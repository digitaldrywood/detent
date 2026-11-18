-- +goose Up
-- +goose StatementBegin
GRANT SELECT ON public.goose_db_version TO detent_platform;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
REVOKE SELECT ON public.goose_db_version FROM detent_platform;
-- +goose StatementEnd
