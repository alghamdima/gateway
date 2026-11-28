-- Reverses 0040 (key lifecycle: ADMIN-12 key defaults, the key limit window functions). Run by
-- hand, then remove the 0040 row from drizzle.__drizzle_migrations. The credits plugin and the API
-- call key_limit_window_start and key_limit_window_end: revert them first.
DROP FUNCTION "public"."key_limit_window_end"(text, timestamp);
DROP FUNCTION "public"."key_limit_window_start"(text, timestamp);
DROP TRIGGER "key_settings_change_append_only_trigger" ON "public"."key_settings_change";
DROP FUNCTION "public"."reject_key_settings_change_change"();
DROP TABLE "key_settings_change";
DROP TABLE "key_settings";
ALTER TABLE "api_key" DROP CONSTRAINT "api_key_expires_after_created";
