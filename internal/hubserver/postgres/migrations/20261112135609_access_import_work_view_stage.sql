-- +goose Up
ALTER TABLE access_imports DROP CONSTRAINT access_imports_stage_check;
ALTER TABLE access_imports ADD CONSTRAINT access_imports_stage_check CHECK (stage BETWEEN 0 AND 21);

-- +goose Down
ALTER TABLE access_imports DROP CONSTRAINT access_imports_stage_check;
ALTER TABLE access_imports ADD CONSTRAINT access_imports_stage_check CHECK (stage BETWEEN 0 AND 20);
