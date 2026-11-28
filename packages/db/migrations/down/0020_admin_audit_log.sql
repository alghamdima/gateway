-- Reverses 0020 (admin audit log). Run by hand, then remove the 0020 row from
-- drizzle.__drizzle_migrations. It deletes the record of what staff did: export it first.
DROP TABLE "public"."admin_audit_log";
DROP FUNCTION "public"."reject_admin_audit_log_change"();
