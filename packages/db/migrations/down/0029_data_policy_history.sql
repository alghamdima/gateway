-- Reverses 0029 (data policy history). Run by hand, then remove the 0029 row from
-- drizzle.__drizzle_migrations. It deletes the record of which policy was in force when:
-- export it first.
DROP TABLE "public"."organization_data_policy_change";
DROP FUNCTION "public"."reject_organization_data_policy_change_change"();
