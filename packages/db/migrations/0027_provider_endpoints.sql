CREATE TABLE "provider_endpoint" (
	"key_id" text PRIMARY KEY NOT NULL,
	"provider" text NOT NULL,
	"region" text NOT NULL,
	"retention_days" integer,
	"trains_on_data" boolean,
	"agreement" text NOT NULL,
	"source" text NOT NULL,
	"verified_on" date NOT NULL,
	"updated_by" text NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "provider_endpoint_region" CHECK ("provider_endpoint"."region" in ('sa', 'eu', 'us', 'other')),
	CONSTRAINT "provider_endpoint_agreement" CHECK ("provider_endpoint"."agreement" in ('direct', 'public_terms', 'private')),
	CONSTRAINT "provider_endpoint_retention_not_negative" CHECK ("provider_endpoint"."retention_days" is null or "provider_endpoint"."retention_days" >= 0)
);
