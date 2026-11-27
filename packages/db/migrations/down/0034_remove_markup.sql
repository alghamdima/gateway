-- Reverses 0034 (no markup on requests). Run by hand, then remove the 0034 row from
-- drizzle.__drizzle_migrations. Revert the API and the gateway first: the gateway would then
-- read the markups again. Markups come back at 0%, which charges at cost as today.
ALTER TABLE "pricing_settings" ADD COLUMN "default_markup_bps" integer DEFAULT 0 NOT NULL;
ALTER TABLE "pricing_settings" ALTER COLUMN "default_markup_bps" DROP DEFAULT;
ALTER TABLE "pricing_settings" ADD CONSTRAINT "pricing_settings_default_markup_range" CHECK ("pricing_settings"."default_markup_bps" between 0 and 100000);
CREATE TABLE "provider_markup" (
	"provider" text PRIMARY KEY NOT NULL,
	"markup_bps" integer NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	"updated_by" text,
	CONSTRAINT "provider_markup_provider_format" CHECK ("provider_markup"."provider" ~ '^[a-z0-9][a-z0-9_.-]{0,63}$'),
	CONSTRAINT "provider_markup_range" CHECK ("provider_markup"."markup_bps" between 0 and 100000)
);
GRANT SELECT ON "public"."provider_markup" TO "gateway_ledger";
