ALTER TABLE "credit_ledger_entry" ADD COLUMN "virtual_key_id" text;--> statement-breakpoint
ALTER TABLE "credit_reservation" ADD COLUMN "virtual_key_id" text;--> statement-breakpoint
CREATE INDEX "credit_ledger_entry_usage_virtualKeyId_createdAt_idx" ON "credit_ledger_entry" USING btree ("virtual_key_id","created_at") WHERE "credit_ledger_entry"."kind" = 'usage' and "credit_ledger_entry"."virtual_key_id" is not null;--> statement-breakpoint
CREATE INDEX "credit_reservation_open_virtualKeyId_idx" ON "credit_reservation" USING btree ("virtual_key_id") WHERE "credit_reservation"."settled_at" is null and "credit_reservation"."virtual_key_id" is not null;--> statement-breakpoint
-- the credits plugin reads only what a key may spend, never its hash or name (ADR 0013)
GRANT SELECT ("virtual_key_id", "limit_micro_usd", "limit_reset") ON "public"."api_key" TO "gateway_ledger";
