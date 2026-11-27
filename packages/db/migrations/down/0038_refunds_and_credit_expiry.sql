-- Reverses 0038 (self-serve refunds, credit lots and expiry: ADMIN-23, BILL-10, BILL-17, ADR 0019).
-- Run by hand, then remove the 0038 row from drizzle.__drizzle_migrations. Revert the API first: it
-- writes lots, refunds and expiry entries. Refused while any ledger entry names a lot (a refund or
-- an expiry): the ledger is append-only, so those entries cannot lose what they record. A test
-- database can be dropped instead. The enum value 'expiry' stays: Postgres cannot drop one, no entry
-- uses it once this script has run, and 0038 adds it with IF NOT EXISTS, so it applies again.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM "credit_ledger_entry" WHERE "credit_lot_id" IS NOT NULL) THEN
    RAISE EXCEPTION 'ledger entries name credit lots: 0038 cannot be reversed';
  END IF;
END $$;
ALTER TABLE "promo_code" DROP CONSTRAINT "promo_code_credit_expiry";
ALTER TABLE "promo_code" DROP COLUMN "credit_expires_at";
ALTER TABLE "pricing_settings" DROP CONSTRAINT "pricing_settings_credit_expiry_range";
ALTER TABLE "pricing_settings" DROP CONSTRAINT "pricing_settings_refund_window_range";
ALTER TABLE "pricing_settings" DROP COLUMN "credit_expiry_days";
ALTER TABLE "pricing_settings" DROP COLUMN "refund_window_hours";
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_refund_is_debit";
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_lot_kind";
DROP INDEX "credit_ledger_entry_creditLotId_idx";
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_credit_lot_id_credit_lot_id_fk";
ALTER TABLE "credit_ledger_entry" DROP COLUMN "credit_lot_id";
DROP TABLE "credit_refund";
DROP TRIGGER "credit_lot_append_only_trigger" ON "credit_lot";
DROP FUNCTION "public"."limit_credit_lot_change"();
DROP TABLE "credit_lot";
