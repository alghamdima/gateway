-- Reverses 0016 (credits). drizzle-kit has no down migrations: run this by hand, then remove the
-- 0016 row from drizzle.__drizzle_migrations. It deletes every ledger entry, so take a backup first.
REVOKE ALL ON "public"."credit_reservation", "public"."credit_account", "public"."credit_ledger_entry" FROM "gateway_ledger";
REVOKE USAGE ON SCHEMA "public" FROM "gateway_ledger";
DROP TABLE "public"."credit_reservation";
DROP TABLE "public"."credit_account";
DROP TABLE "public"."credit_ledger_entry";
DROP FUNCTION "public"."reject_credit_ledger_entry_change"();
DROP TYPE "public"."credit_entry_kind";
-- The role is cluster-wide and may be used by another database: drop it only if nothing else does.
-- DROP ROLE "gateway_ledger";
