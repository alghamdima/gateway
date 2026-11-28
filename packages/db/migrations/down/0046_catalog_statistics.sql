-- Reverses 0046 (the catalog's statistics and settings, SPEC CAT-11, CAT-12). Run by hand, then
-- remove the 0046 row from drizzle.__drizzle_migrations. Revert the API first: the statistics job
-- and the model page read these tables. The hourly rows are lost: hours older than the gateway's
-- log retention cannot be read from the gateway again.
DROP TRIGGER "catalog_settings_change_append_only_trigger" ON "public"."catalog_settings_change";
DROP FUNCTION "public"."reject_catalog_settings_change_change"();
DROP TABLE "catalog_settings_change";
DROP TABLE "catalog_settings";
DROP TABLE "endpoint_stat_state";
DROP TABLE "model_endpoint_stat_hourly";
