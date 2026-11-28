CREATE TABLE "account_settings" (
	"id" smallint PRIMARY KEY DEFAULT 1 NOT NULL,
	"max_owned_organizations" integer DEFAULT 5 NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_by" text,
	CONSTRAINT "account_settings_single_row" CHECK ("account_settings"."id" = 1),
	CONSTRAINT "account_settings_max_owned_organizations_range" CHECK ("account_settings"."max_owned_organizations" between 0 and 1000)
);
--> statement-breakpoint
CREATE TABLE "account_settings_change" (
	"id" bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY (sequence name "account_settings_change_id_seq" INCREMENT BY 1 MINVALUE 1 MAXVALUE 9223372036854775807 START WITH 1 CACHE 1),
	"before" jsonb NOT NULL,
	"after" jsonb NOT NULL,
	"changed_by" text NOT NULL,
	"changed_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "user_organization_limit" (
	"user_id" text PRIMARY KEY NOT NULL,
	"max_owned_organizations" integer NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_by" text,
	CONSTRAINT "user_organization_limit_range" CHECK ("user_organization_limit"."max_owned_organizations" between 0 and 1000)
);
--> statement-breakpoint
ALTER TABLE "user_organization_limit" ADD CONSTRAINT "user_organization_limit_user_id_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "public"."user"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
-- ADR 0020: one user may own 5 organizations to start with (owner, 2026-11-28); staff change it
-- in the admin, and raise or lower it for one user in user_organization_limit. Nobody loses an
-- organization they already own: the limit is checked only when one is created.
INSERT INTO "account_settings" ("id") VALUES (1);--> statement-breakpoint
CREATE OR REPLACE FUNCTION "public"."reject_account_settings_change_change"()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'account_settings_change is append-only: % is not allowed', TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "account_settings_change_append_only_trigger"
BEFORE UPDATE OR DELETE ON "public"."account_settings_change"
FOR EACH ROW
EXECUTE FUNCTION "public"."reject_account_settings_change_change"();
