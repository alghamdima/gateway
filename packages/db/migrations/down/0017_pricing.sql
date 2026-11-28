-- Reverses 0017 (pricing). drizzle-kit has no down migrations: run this by hand, then remove the
-- 0017 row from drizzle.__drizzle_migrations. It deletes the prices staff set and their history.
-- The credits plugin refuses billable requests once pricing_settings is gone.
REVOKE ALL ON "public"."pricing_settings", "public"."provider_markup" FROM "gateway_ledger";
DROP TABLE "public"."pricing_change";
DROP TABLE "public"."provider_markup";
DROP TABLE "public"."pricing_settings";
DROP FUNCTION "public"."reject_pricing_change_change"();
