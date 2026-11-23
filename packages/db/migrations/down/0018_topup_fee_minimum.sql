-- Reverses 0018 (minimum top-up fee). Run by hand, then remove the 0018 row from
-- drizzle.__drizzle_migrations.
ALTER TABLE "pricing_settings" DROP CONSTRAINT "pricing_settings_topup_fee_min_range";
ALTER TABLE "pricing_settings" DROP COLUMN "topup_fee_min_micro_usd";
