-- Reverses 0025 (low-balance alerts). Run by hand, then remove the 0025 row from
-- drizzle.__drizzle_migrations. Organizations' alert settings are lost.
DROP TABLE "public"."low_balance_alert";
