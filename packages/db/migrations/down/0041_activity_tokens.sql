-- Reverses 0041 (ACT-04 token columns). Run by hand, then remove the 0041 row from
-- drizzle.__drizzle_migrations. Revert the credits plugin first: its settle statement writes these
-- columns. Dropping them loses the cached and reasoning counts of every entry written since 0041.
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_tokens_not_negative";
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_tokens_usage_only";
ALTER TABLE "credit_ledger_entry" DROP COLUMN "reasoning_tokens";
ALTER TABLE "credit_ledger_entry" DROP COLUMN "cache_write_tokens";
ALTER TABLE "credit_ledger_entry" DROP COLUMN "cached_tokens";
