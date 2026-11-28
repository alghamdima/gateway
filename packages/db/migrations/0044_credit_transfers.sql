ALTER TYPE "public"."credit_entry_kind" ADD VALUE IF NOT EXISTS 'transfer_out';--> statement-breakpoint
ALTER TYPE "public"."credit_entry_kind" ADD VALUE IF NOT EXISTS 'transfer_in';--> statement-breakpoint
CREATE TABLE "credit_transfer" (
	"id" text PRIMARY KEY NOT NULL,
	"from_organization_id" text NOT NULL,
	"to_organization_id" text NOT NULL,
	"amount_micro_sar" bigint NOT NULL,
	"requested_by" text NOT NULL,
	"idempotency_key" text NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "credit_transfer_idempotencyKey_unique" UNIQUE("idempotency_key"),
	CONSTRAINT "credit_transfer_amount_halalas" CHECK ("credit_transfer"."amount_micro_sar" > 0 and "credit_transfer"."amount_micro_sar" % 10000 = 0),
	CONSTRAINT "credit_transfer_two_accounts" CHECK ("credit_transfer"."from_organization_id" <> "credit_transfer"."to_organization_id")
);
--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_lot_kind";--> statement-breakpoint
ALTER TABLE "credit_lot" DROP CONSTRAINT "credit_lot_source";--> statement-breakpoint
ALTER TABLE "account_settings" ADD COLUMN "transfer_member_tenure_hours" integer DEFAULT 24 NOT NULL;--> statement-breakpoint
ALTER TABLE "account_settings" ADD COLUMN "transfer_cooldown_hours" integer DEFAULT 1 NOT NULL;--> statement-breakpoint
CREATE INDEX "credit_transfer_fromOrganizationId_idx" ON "credit_transfer" USING btree ("from_organization_id");--> statement-breakpoint
CREATE INDEX "credit_transfer_toOrganizationId_createdAt_idx" ON "credit_transfer" USING btree ("to_organization_id","created_at");--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_transfer_in_is_credit" CHECK ("credit_ledger_entry"."kind"::text <> 'transfer_in' or "credit_ledger_entry"."amount" > 0);--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_lot_kind" CHECK (("credit_ledger_entry"."credit_lot_id" is null or "credit_ledger_entry"."kind"::text in ('refund', 'expiry', 'promo_clawback', 'transfer_out')) and ("credit_ledger_entry"."kind"::text not in ('expiry', 'promo_clawback', 'transfer_out') or ("credit_ledger_entry"."credit_lot_id" is not null and "credit_ledger_entry"."amount" < 0)));--> statement-breakpoint
ALTER TABLE "credit_lot" ADD CONSTRAINT "credit_lot_source" CHECK ("credit_lot"."source" in ('top_up', 'promo', 'adjustment', 'transfer'));--> statement-breakpoint
ALTER TABLE "account_settings" ADD CONSTRAINT "account_settings_transfer_waits_range" CHECK ("account_settings"."transfer_member_tenure_hours" between 0 and 8760 and "account_settings"."transfer_cooldown_hours" between 0 and 8760);--> statement-breakpoint
-- Credit transfers (BILL-12, ADR 0020). A transfer is recorded once and never changed: the money
-- is in the ledger, and this row is what a retry finds. The checks compare kind as text, because
-- the enum values are added in this same transaction. The two waits start as OpenRouter has them
-- in kind (a new member waits, an organization that just received waits): 24 hours and 1 hour,
-- changed by staff in the admin.
CREATE OR REPLACE FUNCTION "public"."reject_credit_transfer_change"()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'credit_transfer is append-only: % is not allowed', TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "credit_transfer_append_only_trigger"
BEFORE UPDATE OR DELETE ON "public"."credit_transfer"
FOR EACH ROW
EXECUTE FUNCTION "public"."reject_credit_transfer_change"();
