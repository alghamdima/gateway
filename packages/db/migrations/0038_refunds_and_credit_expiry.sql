ALTER TYPE "public"."credit_entry_kind" ADD VALUE IF NOT EXISTS 'expiry';--> statement-breakpoint
CREATE TABLE "credit_lot" (
	"id" text PRIMARY KEY NOT NULL,
	"organization_id" text NOT NULL,
	"entry_id" text NOT NULL,
	"source" text NOT NULL,
	"granted_micro_sar" bigint NOT NULL,
	"expires_at" timestamp,
	"refundable_until" timestamp,
	"expired_at" timestamp,
	"created_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "credit_lot_entryId_unique" UNIQUE("entry_id"),
	CONSTRAINT "credit_lot_source" CHECK ("credit_lot"."source" in ('top_up', 'promo', 'adjustment')),
	CONSTRAINT "credit_lot_granted_positive" CHECK ("credit_lot"."granted_micro_sar" > 0),
	CONSTRAINT "credit_lot_refund_window" CHECK ("credit_lot"."refundable_until" is null or ("credit_lot"."source" = 'top_up' and "credit_lot"."refundable_until" > "credit_lot"."created_at")),
	CONSTRAINT "credit_lot_expiry" CHECK (("credit_lot"."expires_at" is null or "credit_lot"."expires_at" > "credit_lot"."created_at") and ("credit_lot"."expired_at" is null or "credit_lot"."expires_at" is not null))
);
--> statement-breakpoint
CREATE TABLE "credit_refund" (
	"id" text PRIMARY KEY NOT NULL,
	"organization_id" text NOT NULL,
	"credit_lot_id" text NOT NULL,
	"amount_micro_sar" bigint NOT NULL,
	"status" text DEFAULT 'pending' NOT NULL,
	"provider" text NOT NULL,
	"payment_id" text NOT NULL,
	"provider_refund_id" text,
	"failure" text,
	"requested_by" text NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "credit_refund_creditLotId_unique" UNIQUE("credit_lot_id"),
	CONSTRAINT "credit_refund_providerRefundId_unique" UNIQUE("provider_refund_id"),
	CONSTRAINT "credit_refund_amount_halalas" CHECK ("credit_refund"."amount_micro_sar" > 0 and "credit_refund"."amount_micro_sar" % 10000 = 0),
	CONSTRAINT "credit_refund_status" CHECK ("credit_refund"."status" in ('pending', 'succeeded', 'failed') and ("credit_refund"."status" <> 'succeeded' or "credit_refund"."provider_refund_id" is not null))
);
--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD COLUMN "credit_lot_id" text;--> statement-breakpoint
ALTER TABLE "pricing_settings" ADD COLUMN "refund_window_hours" integer DEFAULT 24 NOT NULL;--> statement-breakpoint
ALTER TABLE "pricing_settings" ADD COLUMN "credit_expiry_days" integer DEFAULT 365;--> statement-breakpoint
ALTER TABLE "promo_code" ADD COLUMN "credit_expires_at" timestamp;--> statement-breakpoint
ALTER TABLE "credit_lot" ADD CONSTRAINT "credit_lot_entry_id_credit_ledger_entry_id_fk" FOREIGN KEY ("entry_id") REFERENCES "public"."credit_ledger_entry"("id") ON DELETE restrict ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "credit_refund" ADD CONSTRAINT "credit_refund_credit_lot_id_credit_lot_id_fk" FOREIGN KEY ("credit_lot_id") REFERENCES "public"."credit_lot"("id") ON DELETE restrict ON UPDATE no action;--> statement-breakpoint
CREATE INDEX "credit_lot_organizationId_createdAt_idx" ON "credit_lot" USING btree ("organization_id","created_at");--> statement-breakpoint
CREATE INDEX "credit_lot_due_expiresAt_idx" ON "credit_lot" USING btree ("expires_at") WHERE "credit_lot"."expired_at" is null and "credit_lot"."expires_at" is not null;--> statement-breakpoint
CREATE INDEX "credit_refund_organizationId_idx" ON "credit_refund" USING btree ("organization_id");--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_credit_lot_id_credit_lot_id_fk" FOREIGN KEY ("credit_lot_id") REFERENCES "public"."credit_lot"("id") ON DELETE restrict ON UPDATE no action;--> statement-breakpoint
CREATE INDEX "credit_ledger_entry_creditLotId_idx" ON "credit_ledger_entry" USING btree ("credit_lot_id") WHERE "credit_ledger_entry"."credit_lot_id" is not null;--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_lot_kind" CHECK (("credit_ledger_entry"."credit_lot_id" is null or "credit_ledger_entry"."kind"::text in ('refund', 'expiry')) and ("credit_ledger_entry"."kind"::text <> 'expiry' or ("credit_ledger_entry"."credit_lot_id" is not null and "credit_ledger_entry"."amount" < 0)));--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_refund_is_debit" CHECK ("credit_ledger_entry"."kind"::text <> 'refund' or "credit_ledger_entry"."amount" < 0);--> statement-breakpoint
ALTER TABLE "pricing_settings" ADD CONSTRAINT "pricing_settings_refund_window_range" CHECK ("pricing_settings"."refund_window_hours" between 0 and 720);--> statement-breakpoint
ALTER TABLE "pricing_settings" ADD CONSTRAINT "pricing_settings_credit_expiry_range" CHECK ("pricing_settings"."credit_expiry_days" is null or "pricing_settings"."credit_expiry_days" between 1 and 3650);--> statement-breakpoint
ALTER TABLE "promo_code" ADD CONSTRAINT "promo_code_credit_expiry" CHECK ("promo_code"."credit_expires_at" is null or ("promo_code"."kind" <> 'fee_discount' and ("promo_code"."ends_at" is null or "promo_code"."credit_expires_at" >= "promo_code"."ends_at") and ("promo_code"."starts_at" is null or "promo_code"."credit_expires_at" > "promo_code"."starts_at")));--> statement-breakpoint
-- Every credit granted before this migration becomes a lot that never expires and offers no
-- self-serve refund: it was bought on terms that had neither (ADMIN-23: a change applies to
-- purchases made after it). ADR 0019.
INSERT INTO "credit_lot" ("id", "organization_id", "entry_id", "source", "granted_micro_sar", "created_at")
SELECT gen_random_uuid()::text, e."organization_id", e."id", e."kind"::text, e."amount", e."created_at"
  FROM "credit_ledger_entry" e
 WHERE e."kind"::text IN ('top_up', 'promo') OR (e."kind"::text = 'adjustment' AND e."amount" > 0);--> statement-breakpoint
-- A lot is a fact: only the expiry job's one mark (expired_at, from null) may change it.
CREATE OR REPLACE FUNCTION "public"."limit_credit_lot_change"()
RETURNS TRIGGER AS $$
BEGIN
  IF TG_OP = 'UPDATE'
     AND OLD."expired_at" IS NULL AND NEW."expired_at" IS NOT NULL
     AND (to_jsonb(NEW) - 'expired_at') = (to_jsonb(OLD) - 'expired_at') THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'credit_lot is append-only: % is not allowed', TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "credit_lot_append_only_trigger"
BEFORE UPDATE OR DELETE ON "public"."credit_lot"
FOR EACH ROW
EXECUTE FUNCTION "public"."limit_credit_lot_change"();
