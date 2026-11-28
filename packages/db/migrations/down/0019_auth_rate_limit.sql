-- Reverses 0019 (shared auth rate limits). Run by hand, then remove the 0019 row from
-- drizzle.__drizzle_migrations. Set rateLimit.storage back to memory first.
DROP TABLE "public"."rate_limit";
