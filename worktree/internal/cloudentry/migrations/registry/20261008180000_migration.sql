-- +goose Up
ALTER TABLE organizations ADD COLUMN checkout_price TEXT NOT NULL DEFAULT '';
