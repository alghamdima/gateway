CREATE TABLE "pricing_change" (
	"id" bigserial PRIMARY KEY NOT NULL,
	"target" text NOT NULL,
	"before" jsonb,
	"after" jsonb,
	"changed_by" text NOT NULL,
	"changed_at" timestamp DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "pricing_settings" (
	"id" smallint PRIMARY KEY DEFAULT 1 NOT NULL,
	"default_markup_bps" integer NOT NULL,
	"topup_fee_bps" integer NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	"updated_by" text,
	CONSTRAINT "pricing_settings_single_row" CHECK ("pricing_settings"."id" = 1),
	CONSTRAINT "pricing_settings_default_markup_range" CHECK ("pricing_settings"."default_markup_bps" between 0 and 100000),
	CONSTRAINT "pricing_settings_topup_fee_range" CHECK ("pricing_settings"."topup_fee_bps" between 0 and 10000)
);
--> statement-breakpoint
CREATE TABLE "provider_markup" (
	"provider" text PRIMARY KEY NOT NULL,
	"markup_bps" integer NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	"updated_by" text,
	CONSTRAINT "provider_markup_provider_format" CHECK ("provider_markup"."provider" ~ '^[a-z0-9][a-z0-9_.-]{0,63}$'),
	CONSTRAINT "provider_markup_range" CHECK ("provider_markup"."markup_bps" between 0 and 100000)
);
--> statement-breakpoint
INSERT INTO "pricing_settings" ("id", "default_markup_bps", "topup_fee_bps") VALUES (1, 0, 550);--> statement-breakpoint
CREATE OR REPLACE FUNCTION "public"."reject_pricing_change_change"()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'pricing_change is append-only: % is not allowed', TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "pricing_change_append_only_trigger"
BEFORE UPDATE OR DELETE ON "public"."pricing_change"
FOR EACH ROW
EXECUTE FUNCTION "public"."reject_pricing_change_change"();--> statement-breakpoint
GRANT SELECT ON "public"."pricing_settings" TO "gateway_ledger";--> statement-breakpoint
GRANT SELECT ON "public"."provider_markup" TO "gateway_ledger";
