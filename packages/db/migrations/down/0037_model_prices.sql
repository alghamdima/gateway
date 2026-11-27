-- Reverses 0037 (model price table, ADMIN-09, ADR 0018). Run by hand, then remove the 0037 row from
-- drizzle.__drizzle_migrations. Revert the API first: it reads and writes these tables. The gateway
-- keeps the pricing overrides the push created (names starting "lemru-price:"); delete them through
-- /api/governance/pricing-overrides to fall back to the datasheet's prices. Usage entries keep the
-- name of the override that priced them in metadata.price_override: the ledger is append-only.
DROP TRIGGER "model_price_append_only_trigger" ON "model_price";
DROP FUNCTION "public"."reject_model_price_change"();
ALTER TABLE "model_price" DROP CONSTRAINT "model_price_suggestion_id_model_price_suggestion_id_fk";
DROP TABLE "model_price_push";
DROP TABLE "model_price_suggestion";
DROP TABLE "model_price";
