-- Reverses 0032 (payment gateway management, ADMIN-03). Run by hand, then remove the 0032 row
-- from drizzle.__drizzle_migrations. The API then has no payment settings to read: revert it first.
DROP TRIGGER "payment_change_append_only_trigger" ON "public"."payment_change";
DROP FUNCTION "public"."reject_payment_change_change"();
DROP TABLE "payment_settings";
DROP TABLE "payment_provider";
DROP TABLE "payment_change";
