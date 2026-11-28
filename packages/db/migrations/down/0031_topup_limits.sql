-- Reverses 0031 (top-up limits and quick picks). Run by hand, then remove the 0031 row from
-- drizzle.__drizzle_migrations. The API then has no limits to read: revert it first.
ALTER TABLE "pricing_settings" DROP CONSTRAINT "pricing_settings_topup_presets_range";
ALTER TABLE "pricing_settings" DROP CONSTRAINT "pricing_settings_topup_limits_range";
ALTER TABLE "pricing_settings" DROP COLUMN "topup_presets_micro_usd";
ALTER TABLE "pricing_settings" DROP COLUMN "topup_max_micro_usd";
ALTER TABLE "pricing_settings" DROP COLUMN "topup_min_micro_usd";
