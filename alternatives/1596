-- +goose Up
CREATE TABLE conversation_prices (
 id INTEGER PRIMARY KEY,
 provider TEXT NOT NULL,
 model TEXT NOT NULL,
 effective_at INTEGER NOT NULL,
 input REAL NOT NULL CHECK(input >= 0),
 cached_input REAL NOT NULL CHECK(cached_input >= 0),
 output REAL NOT NULL CHECK(output >= 0),
 UNIQUE(provider, model, effective_at)
);
INSERT INTO conversation_prices(provider, model, effective_at, input, cached_input, output)
VALUES('openai', 'gpt-6-luna', 0, 0.10, 0.01, 0.50);
CREATE TABLE conversation_usage (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 conversation_id TEXT NOT NULL,
 turn_id TEXT NOT NULL,
 provider TEXT NOT NULL,
 model TEXT NOT NULL,
 occurred_at INTEGER NOT NULL,
 input INTEGER NOT NULL CHECK(input >= 0),
 cached_input INTEGER NOT NULL CHECK(cached_input >= 0 AND cached_input <= input),
 output INTEGER NOT NULL CHECK(output >= 0),
 reasoning_output INTEGER NOT NULL CHECK(reasoning_output >= 0 AND reasoning_output <= output),
 outcome TEXT NOT NULL,
 price_id INTEGER REFERENCES conversation_prices(id),
 cost_usd REAL CHECK(cost_usd >= 0),
 cache_savings_usd REAL CHECK(cache_savings_usd >= 0),
 PRIMARY KEY(organization_id, turn_id)
);
CREATE INDEX conversation_usage_window_idx ON conversation_usage(organization_id, occurred_at, project_id);
-- +goose Down
DROP TABLE conversation_usage;
DROP TABLE conversation_prices;
