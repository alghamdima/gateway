-- ACT-04: the cached, cache-write and reasoning tokens of each usage entry, written by the credits
-- plugin in its one settle statement (apps/gateway/plugins/credits/store.go), so the activity page
-- can sum them for as long as the ledger is kept, not only the gateway log's retention. Nullable:
-- entries before this migration, and entries of every other kind, have none. The gateway_ledger
-- role's INSERT grant is on the table, so it covers the new columns.
ALTER TABLE "credit_ledger_entry" ADD COLUMN "cached_tokens" bigint;--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD COLUMN "cache_write_tokens" bigint;--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD COLUMN "reasoning_tokens" bigint;--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_tokens_usage_only" CHECK (("credit_ledger_entry"."cached_tokens" is null and "credit_ledger_entry"."cache_write_tokens" is null and "credit_ledger_entry"."reasoning_tokens" is null) or "credit_ledger_entry"."kind" = 'usage');--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_tokens_not_negative" CHECK (coalesce("credit_ledger_entry"."cached_tokens", 0) >= 0 and coalesce("credit_ledger_entry"."cache_write_tokens", 0) >= 0 and coalesce("credit_ledger_entry"."reasoning_tokens", 0) >= 0);
