CREATE TABLE "low_balance_alert" (
	"organization_id" text PRIMARY KEY NOT NULL,
	"threshold_micro_usd" bigint,
	"notified_at" timestamp,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "low_balance_alert_threshold_positive" CHECK ("low_balance_alert"."threshold_micro_usd" is null or "low_balance_alert"."threshold_micro_usd" > 0)
);
--> statement-breakpoint
ALTER TABLE "low_balance_alert" ADD CONSTRAINT "low_balance_alert_organization_id_organization_id_fk" FOREIGN KEY ("organization_id") REFERENCES "public"."organization"("id") ON DELETE cascade ON UPDATE no action;