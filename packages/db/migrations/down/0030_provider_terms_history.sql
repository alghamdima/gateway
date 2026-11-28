-- Reverses 0030 (history of provider data terms). Run by hand, then remove the 0030 row from
-- drizzle.__drizzle_migrations. It deletes the record of which terms were in force when:
-- export it first.
DROP TABLE "public"."provider_endpoint_change";
DROP FUNCTION "public"."reject_provider_endpoint_change_change"();
