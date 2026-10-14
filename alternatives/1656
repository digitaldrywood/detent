-- +goose Up
-- The hub's claim predicate was a function of the item's lane alone, so an
-- item whose attempt had already succeeded and whose lane had not moved was
-- offered again on every poll, and every offer was a real claim and a real
-- model turn (operations.md section 7, "An item in an active state is
-- re-dispatched forever").
--
-- Two facts are needed to stop that without stranding legitimate re-runs.
--
-- native_attempts.work_item_revision (00023) is the issues.revision an attempt
-- ran against. An attempt that succeeded at a revision the item has since
-- moved past answered an older version of the item, so the item is offered
-- again; an attempt that succeeded at the current revision already answered
-- this version.
--
-- dispatch_generation is the explicit "run this item again" request. The
-- reason it exists rather than being inferred: a conversation `continue`
-- bumps no revision and changes no lane (decisions.md sections 9 and 10), so
-- a revision-only guard refuses a continuation that the product promises. The
-- generation is bumped wherever a command is accepted that only a new attempt
-- can carry, and an attempt records the generation it was dispatched for, so
-- "a request newer than the last attempt" is an integer comparison rather
-- than a guess about clocks or intent.
ALTER TABLE issues ADD COLUMN dispatch_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE issues ADD COLUMN dispatch_requested_at TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN dispatch_generation INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE native_attempts DROP COLUMN dispatch_generation;
ALTER TABLE issues DROP COLUMN dispatch_requested_at;
ALTER TABLE issues DROP COLUMN dispatch_generation;
