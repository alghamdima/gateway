-- Money in SAR (docs/platform/adr/0017-sar-ledger.md, ADMIN-06, BILL-15). Every amount the
-- ledger, the limits and the money settings hold becomes micro-SAR (1 SAR = 1,000,000). The
-- columns that named their unit are renamed, so the unit stays visible; amount, balance and
-- held keep their names. No deployment holds data yet, so values are not converted: a test
-- database keeps its numbers, now read as SAR.

-- the money settings: renamed, then seeded with the SAR defaults (USD values x 3.75, rounded)
ALTER TABLE "pricing_settings" DROP CONSTRAINT "pricing_settings_topup_fee_min_range";--> statement-breakpoint
ALTER TABLE "pricing_settings" DROP CONSTRAINT "pricing_settings_topup_limits_range";--> statement-breakpoint
ALTER TABLE "pricing_settings" DROP CONSTRAINT "pricing_settings_topup_presets_range";--> statement-breakpoint
ALTER TABLE "pricing_settings" RENAME COLUMN "topup_fee_min_micro_usd" TO "topup_fee_min_micro_sar";--> statement-breakpoint
ALTER TABLE "pricing_settings" RENAME COLUMN "topup_min_micro_usd" TO "topup_min_micro_sar";--> statement-breakpoint
ALTER TABLE "pricing_settings" RENAME COLUMN "topup_max_micro_usd" TO "topup_max_micro_sar";--> statement-breakpoint
ALTER TABLE "pricing_settings" RENAME COLUMN "topup_presets_micro_usd" TO "topup_presets_micro_sar";--> statement-breakpoint
ALTER TABLE "pricing_settings" ALTER COLUMN "topup_fee_min_micro_sar" SET DEFAULT 3000000;--> statement-breakpoint
ALTER TABLE "pricing_settings" ALTER COLUMN "topup_min_micro_sar" SET DEFAULT 20000000;--> statement-breakpoint
ALTER TABLE "pricing_settings" ALTER COLUMN "topup_max_micro_sar" SET DEFAULT 3750000000;--> statement-breakpoint
ALTER TABLE "pricing_settings" ALTER COLUMN "topup_presets_micro_sar" SET DEFAULT '{40000000,100000000,200000000,400000000}'::bigint[];--> statement-breakpoint
UPDATE "pricing_settings" SET
  "topup_fee_min_micro_sar" = 3000000,
  "topup_min_micro_sar" = 20000000,
  "topup_max_micro_sar" = 3750000000,
  "topup_presets_micro_sar" = '{40000000,100000000,200000000,400000000}'::bigint[];--> statement-breakpoint
ALTER TABLE "pricing_settings" ADD CONSTRAINT "pricing_settings_topup_fee_min_range" CHECK ("pricing_settings"."topup_fee_min_micro_sar" between 0 and 400000000);--> statement-breakpoint
ALTER TABLE "pricing_settings" ADD CONSTRAINT "pricing_settings_topup_limits_range" CHECK ("pricing_settings"."topup_min_micro_sar" > 0 and "pricing_settings"."topup_max_micro_sar" >= "pricing_settings"."topup_min_micro_sar" and "pricing_settings"."topup_max_micro_sar" <= 4000000000000);--> statement-breakpoint
ALTER TABLE "pricing_settings" ADD CONSTRAINT "pricing_settings_topup_presets_range" CHECK (cardinality("pricing_settings"."topup_presets_micro_sar") between 1 and 8 and array_position("pricing_settings"."topup_presets_micro_sar", null) is null and "pricing_settings"."topup_min_micro_sar" <= all("pricing_settings"."topup_presets_micro_sar") and "pricing_settings"."topup_max_micro_sar" >= all("pricing_settings"."topup_presets_micro_sar"));--> statement-breakpoint

-- the official USD to SAR rate x 1,000,000 and the bank's spread in bps, set by staff
-- (ADMIN-06); the gateway reads both with the markups and converts at official + spread
ALTER TABLE "pricing_settings" ADD COLUMN "usd_to_sar_rate_e6" integer DEFAULT 3750000 NOT NULL;--> statement-breakpoint
ALTER TABLE "pricing_settings" ADD COLUMN "fx_spread_bps" integer DEFAULT 0 NOT NULL;--> statement-breakpoint
ALTER TABLE "pricing_settings" ADD CONSTRAINT "pricing_settings_usd_to_sar_rate_range" CHECK ("pricing_settings"."usd_to_sar_rate_e6" between 3000000 and 5000000);--> statement-breakpoint
ALTER TABLE "pricing_settings" ADD CONSTRAINT "pricing_settings_fx_spread_range" CHECK ("pricing_settings"."fx_spread_bps" between 0 and 1000);--> statement-breakpoint

-- key limits and low-balance thresholds: renamed. A column grant follows its column, so
-- gateway_ledger keeps SELECT on the renamed limit (migration 0022)
ALTER TABLE "api_key" DROP CONSTRAINT "api_key_limit_positive";--> statement-breakpoint
ALTER TABLE "api_key" DROP CONSTRAINT "api_key_limit_reset_needs_limit";--> statement-breakpoint
ALTER TABLE "api_key" RENAME COLUMN "limit_micro_usd" TO "limit_micro_sar";--> statement-breakpoint
ALTER TABLE "api_key" ADD CONSTRAINT "api_key_limit_positive" CHECK ("api_key"."limit_micro_sar" is null or "api_key"."limit_micro_sar" > 0);--> statement-breakpoint
ALTER TABLE "api_key" ADD CONSTRAINT "api_key_limit_reset_needs_limit" CHECK ("api_key"."limit_reset" is null or "api_key"."limit_micro_sar" is not null);--> statement-breakpoint
ALTER TABLE "low_balance_alert" DROP CONSTRAINT "low_balance_alert_threshold_positive";--> statement-breakpoint
ALTER TABLE "low_balance_alert" RENAME COLUMN "threshold_micro_usd" TO "threshold_micro_sar";--> statement-breakpoint
ALTER TABLE "low_balance_alert" ADD CONSTRAINT "low_balance_alert_threshold_positive" CHECK ("low_balance_alert"."threshold_micro_sar" is null or "low_balance_alert"."threshold_micro_sar" > 0);--> statement-breakpoint

-- a hold records the official rate, the spread and the effective rate it was sized at; its
-- settlement converts at the same rate. Holds taken before this migration were in USD,
-- which is a rate of exactly 1 with no spread
ALTER TABLE "credit_reservation" ADD COLUMN "usd_to_sar_rate_e6" integer DEFAULT 1000000 NOT NULL;--> statement-breakpoint
ALTER TABLE "credit_reservation" ADD COLUMN "fx_spread_bps" integer DEFAULT 0 NOT NULL;--> statement-breakpoint
ALTER TABLE "credit_reservation" ADD COLUMN "effective_usd_to_sar_rate_e6" integer DEFAULT 1000000 NOT NULL;--> statement-breakpoint
ALTER TABLE "credit_reservation" ALTER COLUMN "usd_to_sar_rate_e6" DROP DEFAULT;--> statement-breakpoint
ALTER TABLE "credit_reservation" ALTER COLUMN "fx_spread_bps" DROP DEFAULT;--> statement-breakpoint
ALTER TABLE "credit_reservation" ALTER COLUMN "effective_usd_to_sar_rate_e6" DROP DEFAULT;--> statement-breakpoint
ALTER TABLE "credit_reservation" ADD CONSTRAINT "credit_reservation_usd_to_sar_rate_positive" CHECK ("credit_reservation"."usd_to_sar_rate_e6" > 0);--> statement-breakpoint
ALTER TABLE "credit_reservation" ADD CONSTRAINT "credit_reservation_effective_rate" CHECK ("credit_reservation"."fx_spread_bps" between 0 and 10000 and "credit_reservation"."effective_usd_to_sar_rate_e6"::bigint = ("credit_reservation"."usd_to_sar_rate_e6"::bigint * (10000 + "credit_reservation"."fx_spread_bps") + 9999) / 10000);--> statement-breakpoint

-- a charge records the provider's USD cost, the official rate, the spread and the effective
-- rate it was converted at. The ledger is
-- append-only, so usage entries written before this migration cannot be filled in: the
-- rule is NOT VALID, which leaves them as they are and checks every new row
ALTER TABLE "credit_ledger_entry" ADD COLUMN "cost_micro_usd" bigint;--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD COLUMN "usd_to_sar_rate_e6" integer;--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD COLUMN "fx_spread_bps" integer;--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD COLUMN "effective_usd_to_sar_rate_e6" integer;--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_cost_micro_usd_not_negative" CHECK ("credit_ledger_entry"."cost_micro_usd" is null or "credit_ledger_entry"."cost_micro_usd" >= 0);--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_usd_to_sar_rate_positive" CHECK ("credit_ledger_entry"."usd_to_sar_rate_e6" is null or "credit_ledger_entry"."usd_to_sar_rate_e6" > 0);--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_effective_rate" CHECK ("credit_ledger_entry"."effective_usd_to_sar_rate_e6" is null or ("credit_ledger_entry"."usd_to_sar_rate_e6" is not null and "credit_ledger_entry"."fx_spread_bps" between 0 and 10000 and "credit_ledger_entry"."effective_usd_to_sar_rate_e6"::bigint = ("credit_ledger_entry"."usd_to_sar_rate_e6"::bigint * (10000 + "credit_ledger_entry"."fx_spread_bps") + 9999) / 10000));--> statement-breakpoint
ALTER TABLE "credit_ledger_entry" ADD CONSTRAINT "credit_ledger_entry_usage_has_cost_and_rate" CHECK ("credit_ledger_entry"."kind" <> 'usage' or ("credit_ledger_entry"."cost_micro_usd" is not null and "credit_ledger_entry"."usd_to_sar_rate_e6" is not null and "credit_ledger_entry"."fx_spread_bps" is not null and "credit_ledger_entry"."effective_usd_to_sar_rate_e6" is not null)) NOT VALID;
