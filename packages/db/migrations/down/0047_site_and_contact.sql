-- Reverses 0047 (the site settings and the contact messages, SPEC SITE-01 to SITE-03). Run by
-- hand, then remove the 0047 row from drizzle.__drizzle_migrations. Revert the web app and the
-- API first: the marketing pages read site_settings on every request. The contact messages are
-- lost: export contact_request first if any is still open.
DROP TRIGGER "site_settings_change_append_only_trigger" ON "public"."site_settings_change";
DROP FUNCTION "public"."reject_site_settings_change_change"();
DROP TABLE "site_settings_change";
DROP TABLE "site_settings";
DROP TABLE "contact_request";
