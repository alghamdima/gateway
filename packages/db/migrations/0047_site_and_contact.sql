CREATE TABLE "contact_request" (
	"id" bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY (sequence name "contact_request_id_seq" INCREMENT BY 1 MINVALUE 1 MAXVALUE 9223372036854775807 START WITH 1 CACHE 1),
	"topic" text NOT NULL,
	"name" text NOT NULL,
	"email" text NOT NULL,
	"company" text,
	"message" text NOT NULL,
	"user_id" text,
	"status" text DEFAULT 'new' NOT NULL,
	"staff_note" text,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_by" text,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	CONSTRAINT "contact_request_topic" CHECK ("contact_request"."topic" in ('sales', 'provider', 'support', 'other')),
	CONSTRAINT "contact_request_status" CHECK ("contact_request"."status" in ('new', 'in_progress', 'closed')),
	CONSTRAINT "contact_request_lengths" CHECK (char_length("contact_request"."name") between 1 and 100
        and char_length("contact_request"."email") between 3 and 254
        and ("contact_request"."company" is null or char_length("contact_request"."company") between 1 and 200)
        and char_length("contact_request"."message") between 1 and 4000
        and ("contact_request"."staff_note" is null or char_length("contact_request"."staff_note") <= 4000))
);
--> statement-breakpoint
CREATE TABLE "site_settings" (
	"id" smallint PRIMARY KEY DEFAULT 1 NOT NULL,
	"is_blog_enabled" boolean DEFAULT false NOT NULL,
	"contact_email" text,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_by" text,
	CONSTRAINT "site_settings_single_row" CHECK ("site_settings"."id" = 1),
	CONSTRAINT "site_settings_contact_email_length" CHECK ("site_settings"."contact_email" is null or char_length("site_settings"."contact_email") between 3 and 254)
);
--> statement-breakpoint
CREATE TABLE "site_settings_change" (
	"id" bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY (sequence name "site_settings_change_id_seq" INCREMENT BY 1 MINVALUE 1 MAXVALUE 9223372036854775807 START WITH 1 CACHE 1),
	"before" jsonb NOT NULL,
	"after" jsonb NOT NULL,
	"changed_by" text NOT NULL,
	"changed_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
ALTER TABLE "contact_request" ADD CONSTRAINT "contact_request_user_id_user_id_fk" FOREIGN KEY ("user_id") REFERENCES "public"."user"("id") ON DELETE set null ON UPDATE no action;--> statement-breakpoint
CREATE INDEX "contact_request_status_created_idx" ON "contact_request" USING btree ("status","created_at");--> statement-breakpoint
-- The public site (SPEC SITE-01 to SITE-03, ADMIN-20; owner, 2026-11-28). Staff switch the blog on
-- in the admin: it starts off because it has no posts yet. contact_request keeps every message
-- sent through the contact page, so staff can answer from the admin; site_settings.contact_email
-- is where an email about each new one goes, once staff set it.
INSERT INTO "site_settings" ("id") VALUES (1);--> statement-breakpoint
CREATE OR REPLACE FUNCTION "public"."reject_site_settings_change_change"()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'site_settings_change is append-only: % is not allowed', TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "site_settings_change_append_only_trigger"
BEFORE UPDATE OR DELETE ON "public"."site_settings_change"
FOR EACH ROW
EXECUTE FUNCTION "public"."reject_site_settings_change_change"();
