CREATE TABLE "organization_data_policy" (
	"organization_id" text PRIMARY KEY NOT NULL,
	"require_zero_retention" boolean DEFAULT false NOT NULL,
	"forbid_training" boolean DEFAULT false NOT NULL,
	"allowed_regions" text[],
	"allowed_providers" text[],
	"version" integer DEFAULT 1 NOT NULL,
	"applied_version" integer DEFAULT 0 NOT NULL,
	"updated_by" text NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "organization_data_policy_regions" CHECK ("organization_data_policy"."allowed_regions" is null or (cardinality("organization_data_policy"."allowed_regions") > 0 and "organization_data_policy"."allowed_regions" <@ array['sa', 'eu', 'us', 'other'])),
	CONSTRAINT "organization_data_policy_providers" CHECK ("organization_data_policy"."allowed_providers" is null or cardinality("organization_data_policy"."allowed_providers") > 0),
	CONSTRAINT "organization_data_policy_versions" CHECK ("organization_data_policy"."version" >= 1 and "organization_data_policy"."applied_version" >= 0 and "organization_data_policy"."applied_version" <= "organization_data_policy"."version")
);
--> statement-breakpoint
ALTER TABLE "organization_data_policy" ADD CONSTRAINT "organization_data_policy_organization_id_organization_id_fk" FOREIGN KEY ("organization_id") REFERENCES "public"."organization"("id") ON DELETE cascade ON UPDATE no action;