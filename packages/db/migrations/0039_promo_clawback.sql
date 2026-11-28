ALTER TYPE "public"."credit_entry_kind" ADD VALUE IF NOT EXISTS 'promo_clawback';--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_lot_kind";--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_promo_code";--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_lot_kind" CHECK (("credit_ledger_entry"."credit_lot_id" is null or "credit_ledger_entry"."kind"::text in ('refund', 'expiry', 'promo_clawback')) and ("credit_ledger_entry"."kind"::text not in ('expiry', 'promo_clawback') or ("credit_ledger_entry"."credit_lot_id" is not null and "credit_ledger_entry"."amount" < 0)));--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_promo_code" CHECK (("credit_ledger_entry"."kind"::text not in ('promo', 'promo_clawback') or "credit_ledger_entry"."promo_code_id" is not null) and ("credit_ledger_entry"."promo_code_id" is null or "credit_ledger_entry"."kind"::text in ('promo', 'top_up', 'promo_clawback')));
-- A promo clawback names the bonus lot it takes from and its promo code, and removes credit
-- (ADR 0019, "Refunds take back the unused promo bonus"). The checks compare kind as text, because
-- the enum value is added in this same transaction.
