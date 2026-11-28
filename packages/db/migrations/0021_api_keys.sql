CREATE TABLE "api_key" (
	"id" text PRIMARY KEY NOT NULL,
	"organization_id" text NOT NULL,
	"created_by" text NOT NULL,
	"name" text NOT NULL,
	"virtual_key_id" text NOT NULL,
	"prefix" text NOT NULL,
	"value_hash" text NOT NULL,
	"limit_micro_usd" bigint,
	"limit_reset" text,
	"expires_at" timestamp,
	"disabled_at" timestamp,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "api_key_virtualKeyId_unique" UNIQUE("virtual_key_id"),
	CONSTRAINT "api_key_valueHash_unique" UNIQUE("value_hash"),
	CONSTRAINT "api_key_limit_positive" CHECK ("api_key"."limit_micro_usd" is null or "api_key"."limit_micro_usd" > 0),
	CONSTRAINT "api_key_limit_reset_needs_limit" CHECK ("api_key"."limit_reset" is null or "api_key"."limit_micro_usd" is not null),
	CONSTRAINT "api_key_limit_reset_value" CHECK ("api_key"."limit_reset" is null or "api_key"."limit_reset" in ('daily', 'weekly', 'monthly'))
);
--> statement-breakpoint
CREATE TABLE "gateway_customer" (
	"organization_id" text PRIMARY KEY NOT NULL,
	"customer_id" text NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "gateway_customer_customerId_unique" UNIQUE("customer_id")
);
--> statement-breakpoint
ALTER TABLE "api_key" ADD CONSTRAINT "api_key_organization_id_organization_id_fk" FOREIGN KEY ("organization_id") REFERENCES "public"."organization"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "gateway_customer" ADD CONSTRAINT "gateway_customer_organization_id_organization_id_fk" FOREIGN KEY ("organization_id") REFERENCES "public"."organization"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
CREATE INDEX "api_key_organizationId_idx" ON "api_key" USING btree ("organization_id");--> statement-breakpoint
GRANT SELECT ON "public"."gateway_customer" TO "gateway_ledger";
