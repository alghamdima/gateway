CREATE TABLE "payment_change" (
	"id" bigserial PRIMARY KEY NOT NULL,
	"target" text NOT NULL,
	"before" jsonb,
	"after" jsonb,
	"changed_by" text NOT NULL,
	"changed_at" timestamp DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "payment_provider" (
	"provider" text PRIMARY KEY NOT NULL,
	"enabled" boolean DEFAULT true NOT NULL,
	"mode" text DEFAULT 'live' NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	"updated_by" text,
	CONSTRAINT "payment_provider_known" CHECK ("payment_provider"."provider" in ('stripe')),
	CONSTRAINT "payment_provider_mode" CHECK ("payment_provider"."mode" in ('test', 'live'))
);
--> statement-breakpoint
CREATE TABLE "payment_settings" (
	"id" smallint PRIMARY KEY DEFAULT 1 NOT NULL,
	"topups_enabled" boolean DEFAULT true NOT NULL,
	"active_provider" text DEFAULT 'stripe' NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	"updated_by" text,
	CONSTRAINT "payment_settings_single_row" CHECK ("payment_settings"."id" = 1)
);
--> statement-breakpoint
ALTER TABLE "payment_settings" ADD CONSTRAINT "payment_settings_active_provider_payment_provider_provider_fk" FOREIGN KEY ("active_provider") REFERENCES "public"."payment_provider"("provider") ON DELETE no action ON UPDATE no action;--> statement-breakpoint
INSERT INTO "payment_provider" ("provider", "enabled", "mode") VALUES ('stripe', true, 'live');--> statement-breakpoint
INSERT INTO "payment_settings" ("id", "topups_enabled", "active_provider") VALUES (1, true, 'stripe');--> statement-breakpoint
CREATE OR REPLACE FUNCTION "public"."reject_payment_change_change"()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'payment_change is append-only: % is not allowed', TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "payment_change_append_only_trigger"
BEFORE UPDATE OR DELETE ON "public"."payment_change"
FOR EACH ROW
EXECUTE FUNCTION "public"."reject_payment_change_change"();
