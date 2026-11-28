-- Reverses 0043 (the limit on organizations a user may own, ADR 0020). Run by hand, then remove
-- the 0043 row from drizzle.__drizzle_migrations. Revert the code that reads account_settings
-- first: creating an organization fails without the row.
DROP TABLE "user_organization_limit";
DROP TRIGGER "account_settings_change_append_only_trigger" ON "public"."account_settings_change";
DROP FUNCTION "public"."reject_account_settings_change_change"();
DROP TABLE "account_settings_change";
DROP TABLE "account_settings";
