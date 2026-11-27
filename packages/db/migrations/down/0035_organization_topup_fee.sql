-- Reverses 0035 (top-up fee per organization, ADMIN-24). Run by hand, then remove the 0035 row
-- from drizzle.__drizzle_migrations. Revert the API first: it reads the table and writes the
-- columns. Every organization then pays the default fee; the fee rate each top-up recorded is
-- lost (the fee amount stays in the entry's metadata) and the pricing_change rows with target
-- "organization:<id>" stay as history.
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_topup_fee";
ALTER TABLE "credit_ledger_entry" DROP COLUMN "topup_fee_source";
ALTER TABLE "credit_ledger_entry" DROP COLUMN "topup_fee_min_micro_sar";
ALTER TABLE "credit_ledger_entry" DROP COLUMN "topup_fee_bps";
DROP TABLE "organization_topup_fee";
