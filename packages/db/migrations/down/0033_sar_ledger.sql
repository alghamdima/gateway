-- Reverses 0033 (money in SAR). Run by hand, then remove the 0033 row from
-- drizzle.__drizzle_migrations. Revert the API and the gateway first: they read the new
-- columns. Amounts are not converted back, as they were not converted forward.
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_usage_has_cost_and_rate";
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_effective_rate";
ALTER TABLE "credit_ledger_entry" DROP COLUMN "effective_usd_to_sar_rate_e6";
ALTER TABLE "credit_ledger_entry" DROP COLUMN "fx_spread_bps";
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_usd_to_sar_rate_positive";
ALTER TABLE "credit_ledger_entry" DROP CONSTRAINT "credit_ledger_entry_cost_micro_usd_not_negative";
ALTER TABLE "credit_ledger_entry" DROP COLUMN "usd_to_sar_rate_e6";
ALTER TABLE "credit_ledger_entry" DROP COLUMN "cost_micro_usd";
ALTER TABLE "credit_reservation" DROP CONSTRAINT "credit_reservation_effective_rate";
ALTER TABLE "credit_reservation" DROP COLUMN "effective_usd_to_sar_rate_e6";
ALTER TABLE "credit_reservation" DROP COLUMN "fx_spread_bps";
ALTER TABLE "credit_reservation" DROP CONSTRAINT "credit_reservation_usd_to_sar_rate_positive";
ALTER TABLE "credit_reservation" DROP COLUMN "usd_to_sar_rate_e6";
ALTER TABLE "low_balance_alert" DROP CONSTRAINT "low_balance_alert_threshold_positive";
ALTER TABLE "low_balance_alert" RENAME COLUMN "threshold_micro_sar" TO "threshold_micro_usd";
ALTER TABLE "low_balance_alert" ADD CONSTRAINT "low_balance_alert_threshold_positive" CHECK ("low_balance_alert"."threshold_micro_usd" is null or "low_balance_alert"."threshold_micro_usd" > 0);
ALTER TABLE "api_key" DROP CONSTRAINT "api_key_limit_reset_needs_limit";
ALTER TABLE "api_key" DROP CONSTRAINT "api_key_limit_positive";
ALTER TABLE "api_key" RENAME COLUMN "limit_micro_sar" TO "limit_micro_usd";
ALTER TABLE "api_key" ADD CONSTRAINT "api_key_limit_positive" CHECK ("api_key"."limit_micro_usd" is null or "api_key"."limit_micro_usd" > 0);
ALTER TABLE "api_key" ADD CONSTRAINT "api_key_limit_reset_needs_limit" CHECK ("api_key"."limit_reset" is null or "api_key"."limit_micro_usd" is not null);
ALTER TABLE "pricing_settings" DROP CONSTRAINT "pricing_settings_fx_spread_range";
ALTER TABLE "pricing_settings" DROP COLUMN "fx_spread_bps";
ALTER TABLE "pricing_settings" DROP CONSTRAINT "pricing_settings_usd_to_sar_rate_range";
ALTER TABLE "pricing_settings" DROP COLUMN "usd_to_sar_rate_e6";
ALTER TABLE "pricing_settings" DROP CONSTRAINT "pricing_settings_topup_presets_range";
ALTER TABLE "pricing_settings" DROP CONSTRAINT "pricing_settings_topup_limits_range";
ALTER TABLE "pricing_settings" DROP CONSTRAINT "pricing_settings_topup_fee_min_range";
ALTER TABLE "pricing_settings" RENAME COLUMN "topup_fee_min_micro_sar" TO "topup_fee_min_micro_usd";
ALTER TABLE "pricing_settings" RENAME COLUMN "topup_min_micro_sar" TO "topup_min_micro_usd";
ALTER TABLE "pricing_settings" RENAME COLUMN "topup_max_micro_sar" TO "topup_max_micro_usd";
ALTER TABLE "pricing_settings" RENAME COLUMN "topup_presets_micro_sar" TO "topup_presets_micro_usd";
ALTER TABLE "pricing_settings" ALTER COLUMN "topup_fee_min_micro_usd" SET DEFAULT 800000;
ALTER TABLE "pricing_settings" ALTER COLUMN "topup_min_micro_usd" SET DEFAULT 5000000;
ALTER TABLE "pricing_settings" ALTER COLUMN "topup_max_micro_usd" SET DEFAULT 1000000000;
ALTER TABLE "pricing_settings" ALTER COLUMN "topup_presets_micro_usd" SET DEFAULT '{10000000,25000000,50000000,100000000}'::bigint[];
UPDATE "pricing_settings" SET
  "topup_fee_min_micro_usd" = 800000,
  "topup_min_micro_usd" = 5000000,
  "topup_max_micro_usd" = 1000000000,
  "topup_presets_micro_usd" = '{10000000,25000000,50000000,100000000}'::bigint[];
ALTER TABLE "pricing_settings" ADD CONSTRAINT "pricing_settings_topup_fee_min_range" CHECK ("pricing_settings"."topup_fee_min_micro_usd" between 0 and 100000000);
ALTER TABLE "pricing_settings" ADD CONSTRAINT "pricing_settings_topup_limits_range" CHECK ("pricing_settings"."topup_min_micro_usd" > 0 and "pricing_settings"."topup_max_micro_usd" >= "pricing_settings"."topup_min_micro_usd" and "pricing_settings"."topup_max_micro_usd" <= 1000000000000);
ALTER TABLE "pricing_settings" ADD CONSTRAINT "pricing_settings_topup_presets_range" CHECK (cardinality("pricing_settings"."topup_presets_micro_usd") between 1 and 8 and array_position("pricing_settings"."topup_presets_micro_usd", null) is null and "pricing_settings"."topup_min_micro_usd" <= all("pricing_settings"."topup_presets_micro_usd") and "pricing_settings"."topup_max_micro_usd" >= all("pricing_settings"."topup_presets_micro_usd"));
