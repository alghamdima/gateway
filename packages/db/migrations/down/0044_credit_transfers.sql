-- Reverses 0044 (credit transfers, BILL-12, ADR 0020). Run by hand, then remove the 0044 row from
-- drizzle.__drizzle_migrations. Revert the API first: it writes transfer entries. Refused while
-- any transfer exists: the ledger is append-only, so those entries cannot lose what they record.
-- The enum values 'transfer_out' and 'transfer_in' stay: Postgres cannot drop one, no entry uses
-- them once this script has run, and 0044 adds them with IF NOT EXISTS, so it applies again.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM "credit_ledger_entry" WHERE "kind"::text IN ('transfer_out', 'transfer_in'))
     OR EXISTS (SELECT 1 FROM "credit_lot" WHERE "source" = 'transfer') THEN
    RAISE EXCEPTION 'credit transfers exist: 0044 cannot be reversed';
  END IF;
END $$;
ALTER TABLE "account_settings" DROP CONSTRAINT "account_settings_transfer_waits_range";
ALTER TABLE "account_settings" DROP COLUMN "transfer_cooldown_hours";
ALTER TABLE "account_settings" DROP COLUMN "transfer_member_tenure_hours";
ALTER TABLE "credit_lot" DROP CONSTRAINT "credit_lot_source";
ALTER TABLE "credit_lot" ADD CONSTRAINT "credit_lot_source" CHECK ("credit_lot"."source" in ('top_up', 'promo', 'adjustment'));
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_lot_kind";
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_lot_kind" CHECK (("credit_ledger_entry"."credit_lot_id" is null or "credit_ledger_entry"."kind"::text in ('refund', 'expiry', 'promo_clawback')) and ("credit_ledger_entry"."kind"::text not in ('expiry', 'promo_clawback') or ("credit_ledger_entry"."credit_lot_id" is not null and "credit_ledger_entry"."amount" < 0)));
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_transfer_in_is_credit";
DROP TRIGGER "credit_transfer_append_only_trigger" ON "public"."credit_transfer";
DROP FUNCTION "public"."reject_credit_transfer_change"();
DROP TABLE "credit_transfer";
