-- Reverses 0023 (organization rate limits). Run by hand, then remove the 0023 row from
-- drizzle.__drizzle_migrations. The gateway keeps the customers' rate limits: clear them
-- there too, or they stay enforced with no record here.
DROP TABLE "public"."organization_rate_limit";
