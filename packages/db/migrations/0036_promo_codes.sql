CREATE TABLE "promo_code" (
	"id" text PRIMARY KEY NOT NULL,
	"code" text NOT NULL,
	"kind" text NOT NULL,
	"fee_discount_bps" integer,
	"fee_discount_micro_sar" bigint,
	"bonus_bps" integer,
	"bonus_cap_micro_sar" bigint,
	"credit_micro_sar" bigint,
	"max_uses" integer,
	"uses" integer DEFAULT 0 NOT NULL,
	"one_per_organization" boolean DEFAULT true NOT NULL,
	"starts_at" timestamp,
	"ends_at" timestamp,
	"min_topup_micro_sar" bigint,
	"all_organizations" boolean DEFAULT true NOT NULL,
	"is_paused" boolean DEFAULT false NOT NULL,
	"note" text DEFAULT '' NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"created_by" text NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	"updated_by" text NOT NULL,
	CONSTRAINT "promo_code_code_format" CHECK ("promo_code"."code" ~ '^[A-Z0-9_-]{3,32}$'),
	CONSTRAINT "promo_code_kind" CHECK ("promo_code"."kind" in ('fee_discount', 'topup_bonus', 'free_credit')),
	CONSTRAINT "promo_code_amounts_match_kind" CHECK (("promo_code"."kind" = 'fee_discount' and (("promo_code"."fee_discount_bps" is null) <> ("promo_code"."fee_discount_micro_sar" is null)) and "promo_code"."bonus_bps" is null and "promo_code"."bonus_cap_micro_sar" is null and "promo_code"."credit_micro_sar" is null)
        or ("promo_code"."kind" = 'topup_bonus' and "promo_code"."bonus_bps" is not null and "promo_code"."bonus_cap_micro_sar" is not null and "promo_code"."fee_discount_bps" is null and "promo_code"."fee_discount_micro_sar" is null and "promo_code"."credit_micro_sar" is null)
        or ("promo_code"."kind" = 'free_credit' and "promo_code"."credit_micro_sar" is not null and "promo_code"."fee_discount_bps" is null and "promo_code"."fee_discount_micro_sar" is null and "promo_code"."bonus_bps" is null and "promo_code"."bonus_cap_micro_sar" is null and "promo_code"."min_topup_micro_sar" is null)),
	CONSTRAINT "promo_code_bps_range" CHECK (("promo_code"."fee_discount_bps" is null or "promo_code"."fee_discount_bps" between 1 and 10000) and ("promo_code"."bonus_bps" is null or "promo_code"."bonus_bps" between 1 and 10000)),
	CONSTRAINT "promo_code_amounts_halalas" CHECK (("promo_code"."fee_discount_micro_sar" is null or ("promo_code"."fee_discount_micro_sar" > 0 and "promo_code"."fee_discount_micro_sar" <= 10000000000 and "promo_code"."fee_discount_micro_sar" % 10000 = 0)) and ("promo_code"."bonus_cap_micro_sar" is null or ("promo_code"."bonus_cap_micro_sar" > 0 and "promo_code"."bonus_cap_micro_sar" <= 10000000000 and "promo_code"."bonus_cap_micro_sar" % 10000 = 0)) and ("promo_code"."credit_micro_sar" is null or ("promo_code"."credit_micro_sar" > 0 and "promo_code"."credit_micro_sar" <= 10000000000 and "promo_code"."credit_micro_sar" % 10000 = 0)) and ("promo_code"."min_topup_micro_sar" is null or ("promo_code"."min_topup_micro_sar" > 0 and "promo_code"."min_topup_micro_sar" <= 10000000000 and "promo_code"."min_topup_micro_sar" % 10000 = 0))),
	CONSTRAINT "promo_code_uses" CHECK ("promo_code"."uses" >= 0 and ("promo_code"."max_uses" is null or "promo_code"."max_uses" > 0)),
	CONSTRAINT "promo_code_dates" CHECK ("promo_code"."starts_at" is null or "promo_code"."ends_at" is null or "promo_code"."ends_at" > "promo_code"."starts_at"),
	CONSTRAINT "promo_code_note_length" CHECK (char_length("promo_code"."note") <= 500)
);
--> statement-breakpoint
CREATE TABLE "promo_code_change" (
	"id" bigserial PRIMARY KEY NOT NULL,
	"promo_code_id" text NOT NULL,
	"before" jsonb,
	"after" jsonb,
	"changed_by" text NOT NULL,
	"changed_at" timestamp DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "promo_code_organization" (
	"promo_code_id" text NOT NULL,
	"organization_id" text NOT NULL,
	CONSTRAINT "promo_code_organization_promo_code_id_organization_id_pk" PRIMARY KEY("promo_code_id","organization_id")
);
--> statement-breakpoint
CREATE TABLE "promo_code_redemption" (
	"id" text PRIMARY KEY NOT NULL,
	"promo_code_id" text NOT NULL,
	"organization_id" text NOT NULL,
	"kind" text NOT NULL,
	"one_per_organization" boolean NOT NULL,
	"payment_id" text,
	"topup_credit_micro_sar" bigint,
	"fee_discount_micro_sar" bigint DEFAULT 0 NOT NULL,
	"credit_micro_sar" bigint DEFAULT 0 NOT NULL,
	"ledger_entry_id" text,
	"topup_entry_id" text,
	"idempotency_key" text NOT NULL,
	"redeemed_by" text,
	"created_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "promo_code_redemption_idempotencyKey_unique" UNIQUE("idempotency_key"),
	CONSTRAINT "promo_code_redemption_amounts" CHECK ("promo_code_redemption"."fee_discount_micro_sar" >= 0 and "promo_code_redemption"."credit_micro_sar" >= 0 and ("promo_code_redemption"."topup_credit_micro_sar" is null or "promo_code_redemption"."topup_credit_micro_sar" > 0))
);
--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_topup_fee";--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD COLUMN "promo_code_id" text;--> statement-breakpoint
ALTER TABLE "promo_code_organization" ADD CONSTRAINT "promo_code_organization_promo_code_id_promo_code_id_fk" FOREIGN KEY ("promo_code_id") REFERENCES "public"."promo_code"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "promo_code_organization" ADD CONSTRAINT "promo_code_organization_organization_id_organization_id_fk" FOREIGN KEY ("organization_id") REFERENCES "public"."organization"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "promo_code_redemption" ADD CONSTRAINT "promo_code_redemption_promo_code_id_promo_code_id_fk" FOREIGN KEY ("promo_code_id") REFERENCES "public"."promo_code"("id") ON DELETE restrict ON UPDATE no action;--> statement-breakpoint
CREATE UNIQUE INDEX "promo_code_code_unique" ON "promo_code" USING btree ("code");--> statement-breakpoint
CREATE INDEX "promo_code_change_promoCodeId_idx" ON "promo_code_change" USING btree ("promo_code_id");--> statement-breakpoint
CREATE UNIQUE INDEX "promo_code_redemption_once_per_organization" ON "promo_code_redemption" USING btree ("promo_code_id","organization_id") WHERE "promo_code_redemption"."one_per_organization";--> statement-breakpoint
CREATE INDEX "promo_code_redemption_promoCodeId_createdAt_idx" ON "promo_code_redemption" USING btree ("promo_code_id","created_at");--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_promo_code_id_promo_code_id_fk" FOREIGN KEY ("promo_code_id") REFERENCES "public"."promo_code"("id") ON DELETE restrict ON UPDATE no action;--> statement-breakpoint
CREATE INDEX "credit_ledger_entry_promoCodeId_idx" ON "credit_ledger_entry" USING btree ("promo_code_id") WHERE "credit_ledger_entry"."promo_code_id" is not null;--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_promo_code" CHECK (("credit_ledger_entry"."kind" <> 'promo' or "credit_ledger_entry"."promo_code_id" is not null) and ("credit_ledger_entry"."promo_code_id" is null or "credit_ledger_entry"."kind" in ('promo', 'top_up')));--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_topup_fee" CHECK (("credit_ledger_entry"."topup_fee_bps" is null and "credit_ledger_entry"."topup_fee_min_micro_sar" is null and "credit_ledger_entry"."topup_fee_source" is null) or ("credit_ledger_entry"."kind" = 'top_up' and "credit_ledger_entry"."topup_fee_bps" >= 0 and "credit_ledger_entry"."topup_fee_min_micro_sar" >= 0 and "credit_ledger_entry"."topup_fee_source" in ('default', 'organization', 'promo')));--> statement-breakpoint
CREATE OR REPLACE FUNCTION "public"."reject_promo_code_change_change"()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'promo_code_change is append-only: % is not allowed', TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "promo_code_change_append_only_trigger"
BEFORE UPDATE OR DELETE ON "public"."promo_code_change"
FOR EACH ROW
EXECUTE FUNCTION "public"."reject_promo_code_change_change"();
