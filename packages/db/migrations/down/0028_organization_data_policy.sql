-- Reverses 0028 (organization data policies). Run by hand, then remove the 0028 row from
-- drizzle.__drizzle_migrations. First set every key of every organization back to all
-- providers in the gateway: the allowlists the policies wrote stay on the keys otherwise.
DROP TABLE "public"."organization_data_policy";
