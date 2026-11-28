CREATE TABLE "organization_topup_fee" (
	"organization_id" text PRIMARY KEY NOT NULL,
	"fee_bps" integer,
	"fee_min_micro_sar" bigint,
	"expires_at" timestamp,
	"note" text DEFAULT '' NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	"updated_by" text NOT NULL,
	CONSTRAINT "organization_topup_fee_bps_range" CHECK ("organization_topup_fee"."fee_bps" is null or "organization_topup_fee"."fee_bps" between 0 and 10000),
	CONSTRAINT "organization_topup_fee_min_range" CHECK ("organization_topup_fee"."fee_min_micro_sar" is null or "organization_topup_fee"."fee_min_micro_sar" between 0 and 400000000),
	CONSTRAINT "organization_topup_fee_sets_something" CHECK ("organization_topup_fee"."fee_bps" is not null or "organization_topup_fee"."fee_min_micro_sar" is not null),
	CONSTRAINT "organization_topup_fee_note_length" CHECK (char_length("organization_topup_fee"."note") <= 500)
);
--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD COLUMN "topup_fee_bps" integer;--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD COLUMN "topup_fee_min_micro_sar" bigint;--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD COLUMN "topup_fee_source" text;--> statement-breakpoint
ALTER TABLE "organization_topup_fee" ADD CONSTRAINT "organization_topup_fee_organization_id_organization_id_fk" FOREIGN KEY ("organization_id") REFERENCES "public"."organization"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_topup_fee" CHECK (("credit_ledger_entry"."topup_fee_bps" is null and "credit_ledger_entry"."topup_fee_min_micro_sar" is null and "credit_ledger_entry"."topup_fee_source" is null) or ("credit_ledger_entry"."kind" = 'top_up' and "credit_ledger_entry"."topup_fee_bps" >= 0 and "credit_ledger_entry"."topup_fee_min_micro_sar" >= 0 and "credit_ledger_entry"."topup_fee_source" in ('default', 'organization')));