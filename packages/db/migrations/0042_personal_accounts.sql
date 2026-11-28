CREATE TABLE "personal_account" (
	"user_id" text PRIMARY KEY NOT NULL,
	"organization_id" text NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "personal_account_organization_id_unique" UNIQUE("organization_id")
);
--> statement-breakpoint
ALTER TABLE "personal_account" ADD CONSTRAINT "personal_account_user_id_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "public"."user"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "personal_account" ADD CONSTRAINT "personal_account_organization_id_organization_id_fk" FOREIGN KEY ("organization_id") REFERENCES "public"."organization"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
-- ADR 0020: every existing user gets an empty personal account: an organization row, the user as
-- its only member (owner), and the personal_account row that marks it. New users get theirs when
-- the user is created (packages/auth/src/lib/personal.ts). Existing organizations are untouched.
WITH "new_account" AS MATERIALIZED (
  SELECT "u"."id" AS "user_id", replace(gen_random_uuid()::text, '-', '') AS "organization_id"
  FROM "user" "u"
), "new_organization" AS (
  INSERT INTO "organization" ("id", "name", "slug", "created_at")
  SELECT "organization_id", 'Personal', 'personal-' || "organization_id", now() FROM "new_account"
), "new_member" AS (
  INSERT INTO "member" ("id", "organization_id", "user_id", "role", "created_at")
  SELECT replace(gen_random_uuid()::text, '-', ''), "organization_id", "user_id", 'owner', now() FROM "new_account"
)
INSERT INTO "personal_account" ("user_id", "organization_id")
SELECT "user_id", "organization_id" FROM "new_account";
