CREATE TABLE "organization_data_policy_change" (
	"id" bigserial PRIMARY KEY NOT NULL,
	"organization_id" text NOT NULL,
	"version" integer NOT NULL,
	"require_zero_retention" boolean NOT NULL,
	"forbid_training" boolean NOT NULL,
	"allowed_regions" text[],
	"allowed_providers" text[],
	"changed_by" text NOT NULL,
	"changed_at" timestamp DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE INDEX "organization_data_policy_change_organization_idx" ON "organization_data_policy_change" USING btree ("organization_id","changed_at");--> statement-breakpoint
CREATE OR REPLACE FUNCTION "public"."reject_organization_data_policy_change_change"()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'organization_data_policy_change is append-only: % is not allowed', TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "organization_data_policy_change_append_only_trigger"
BEFORE UPDATE OR DELETE ON "public"."organization_data_policy_change"
FOR EACH ROW
EXECUTE FUNCTION "public"."reject_organization_data_policy_change_change"();--> statement-breakpoint
-- policies set before this table existed: their current version, as of when it was set
INSERT INTO "public"."organization_data_policy_change"
  ("organization_id", "version", "require_zero_retention", "forbid_training", "allowed_regions", "allowed_providers", "changed_by", "changed_at")
SELECT "organization_id", "version", "require_zero_retention", "forbid_training", "allowed_regions", "allowed_providers", "updated_by", "updated_at"
FROM "public"."organization_data_policy";
