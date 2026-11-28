-- Reverses 0026 (gateway_ledger limits). Run by hand, then remove the 0026 row from
-- drizzle.__drizzle_migrations.
DROP TRIGGER "credit_ledger_entry_gateway_ledger_limit_trigger" ON "public"."credit_ledger_entry";
DROP FUNCTION "public"."limit_gateway_ledger_entry"();
DROP TRIGGER "credit_account_gateway_ledger_limit_trigger" ON "public"."credit_account";
DROP FUNCTION "public"."limit_gateway_ledger_account"();
REVOKE UPDATE ("balance", "held", "updated_at") ON "public"."credit_account" FROM "gateway_ledger";
GRANT UPDATE ON "public"."credit_account" TO "gateway_ledger";
REVOKE UPDATE ("settled_at", "expires_at") ON "public"."credit_reservation" FROM "gateway_ledger";
GRANT UPDATE ON "public"."credit_reservation" TO "gateway_ledger";
