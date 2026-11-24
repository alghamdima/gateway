CREATE TABLE "provider_endpoint_change" (
	"id" bigserial PRIMARY KEY NOT NULL,
	"key_id" text NOT NULL,
	"provider" text NOT NULL,
	"is_removed" boolean DEFAULT false NOT NULL,
	"region" text,
	"retention_days" integer,
	"trains_on_data" boolean,
	"agreement" text,
	"source" text,
	"verified_on" date,
	"changed_by" text NOT NULL,
	"changed_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "provider_endpoint_change_removal_has_no_terms" CHECK (case when "provider_endpoint_change"."is_removed"
        then "provider_endpoint_change"."region" is null and "provider_endpoint_change"."retention_days" is null and "provider_endpoint_change"."trains_on_data" is null
          and "provider_endpoint_change"."agreement" is null and "provider_endpoint_change"."source" is null and "provider_endpoint_change"."verified_on" is null
        else "provider_endpoint_change"."region" is not null and "provider_endpoint_change"."agreement" is not null
          and "provider_endpoint_change"."source" is not null and "provider_endpoint_change"."verified_on" is not null
      end)
);
--> statement-breakpoint
CREATE INDEX "provider_endpoint_change_key_idx" ON "provider_endpoint_change" USING btree ("key_id","changed_at");--> statement-breakpoint
CREATE OR REPLACE FUNCTION "public"."reject_provider_endpoint_change_change"()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'provider_endpoint_change is append-only: % is not allowed', TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "provider_endpoint_change_append_only_trigger"
BEFORE UPDATE OR DELETE ON "public"."provider_endpoint_change"
FOR EACH ROW
EXECUTE FUNCTION "public"."reject_provider_endpoint_change_change"();--> statement-breakpoint
-- terms recorded before this table existed: as they stand, as of when they were last set
INSERT INTO "public"."provider_endpoint_change"
  ("key_id", "provider", "region", "retention_days", "trains_on_data", "agreement", "source", "verified_on", "changed_by", "changed_at")
SELECT "key_id", "provider", "region", "retention_days", "trains_on_data", "agreement", "source", "verified_on", "updated_by", "updated_at"
FROM "public"."provider_endpoint";
