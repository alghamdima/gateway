-- Reverses 0021 (API keys). Run by hand, then remove the 0021 row from
-- drizzle.__drizzle_migrations. The gateway keeps its customers and virtual keys:
-- delete them there first, or every key keeps working and cannot be charged.
REVOKE ALL ON "public"."gateway_customer" FROM "gateway_ledger";
DROP TABLE "public"."api_key";
DROP TABLE "public"."gateway_customer";
