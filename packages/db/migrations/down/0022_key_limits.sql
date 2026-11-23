-- Reverses 0022 (per-key limits in the ledger). Run by hand, then remove the 0022 row
-- from drizzle.__drizzle_migrations. Stop the gateway first: the credits plugin of
-- this version writes these columns. Key limits are then not enforced until the
-- gateway's virtual key budgets are set again.
REVOKE SELECT ("virtual_key_id", "limit_micro_usd", "limit_reset") ON "public"."api_key" FROM "gateway_ledger";
DROP INDEX "public"."credit_reservation_open_virtualKeyId_idx";
DROP INDEX "public"."credit_ledger_entry_usage_virtualKeyId_createdAt_idx";
ALTER TABLE "credit_reservation" DROP COLUMN "virtual_key_id";
ALTER TABLE "credit_ledger_entry" DROP COLUMN "virtual_key_id";
