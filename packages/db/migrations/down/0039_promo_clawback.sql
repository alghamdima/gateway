-- Reverses 0039 (promo clawback on refund, ADR 0019). Run by hand, then remove the 0039 row from
-- drizzle.__drizzle_migrations. Revert the API first: it writes promo_clawback entries. Refused
-- while any entry is a promo clawback: the ledger is append-only, so those entries cannot lose
-- what they record. The enum value 'promo_clawback' stays: Postgres cannot drop one, no entry uses
-- it once this script has run, and 0039 adds it with IF NOT EXISTS, so it applies again.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM "credit_ledger_entry" WHERE "kind"::text = 'promo_clawback') THEN
    RAISE EXCEPTION 'promo clawback entries exist: 0039 cannot be reversed';
  END IF;
END $$;
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_promo_code";
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_lot_kind";
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_lot_kind" CHECK (("credit_ledger_entry"."credit_lot_id" is null or "credit_ledger_entry"."kind"::text in ('refund', 'expiry')) and ("credit_ledger_entry"."kind"::text <> 'expiry' or ("credit_ledger_entry"."credit_lot_id" is not null and "credit_ledger_entry"."amount" < 0)));
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_promo_code" CHECK (("credit_ledger_entry"."kind" <> 'promo' or "credit_ledger_entry"."promo_code_id" is not null) and ("credit_ledger_entry"."promo_code_id" is null or "credit_ledger_entry"."kind" in ('promo', 'top_up')));
