-- Reverses 0027 (provider data terms). Run by hand, then remove the 0027 row from
-- drizzle.__drizzle_migrations. It deletes what staff recorded about each provider key.
DROP TABLE "public"."provider_endpoint";
