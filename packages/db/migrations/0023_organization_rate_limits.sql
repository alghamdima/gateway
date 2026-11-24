CREATE TABLE "organization_rate_limit" (
	"organization_id" text PRIMARY KEY NOT NULL,
	"requests_per_minute" integer,
	"tokens_per_minute" bigint,
	"updated_by" text NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "organization_rate_limit_requests_positive" CHECK ("organization_rate_limit"."requests_per_minute" is null or "organization_rate_limit"."requests_per_minute" > 0),
	CONSTRAINT "organization_rate_limit_tokens_positive" CHECK ("organization_rate_limit"."tokens_per_minute" is null or "organization_rate_limit"."tokens_per_minute" > 0)
);
--> statement-breakpoint
ALTER TABLE "organization_rate_limit" ADD CONSTRAINT "organization_rate_limit_organization_id_organization_id_fk" FOREIGN KEY ("organization_id") REFERENCES "public"."organization"("id") ON DELETE cascade ON UPDATE no action;