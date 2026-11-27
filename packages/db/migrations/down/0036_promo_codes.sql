-- Reverses 0036 (promo codes, BILL-08). Run by hand, then remove the 0036 row from
-- drizzle.__drizzle_migrations. Revert the API first: it reads the tables and writes the column.
-- Refused while any ledger entry names a code or says its fee came from a promo code: the ledger is
-- append-only, so those entries cannot lose what they record. A test database can be dropped instead.
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_topup_fee";
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_topup_fee" CHECK (("credit_ledger_entry"."topup_fee_bps" is null and "credit_ledger_entry"."topup_fee_min_micro_sar" is null and "credit_ledger_entry"."topup_fee_source" is null) or ("credit_ledger_entry"."kind" = 'top_up' and "credit_ledger_entry"."topup_fee_bps" >= 0 and "credit_ledger_entry"."topup_fee_min_micro_sar" >= 0 and "credit_ledger_entry"."topup_fee_source" in ('default', 'organization')));
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_promo_code";
DROP INDEX "credit_ledger_entry_promoCodeId_idx";
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_promo_code_id_promo_code_id_fk";
ALTER TABLE "credit_ledger_entry" DROP COLUMN "promo_code_id";
DROP TRIGGER "promo_code_change_append_only_trigger" ON "promo_code_change";
DROP FUNCTION "public"."reject_promo_code_change_change"();
DROP TABLE "promo_code_change";
DROP TABLE "promo_code_redemption";
DROP TABLE "promo_code_organization";
DROP TABLE "promo_code";
