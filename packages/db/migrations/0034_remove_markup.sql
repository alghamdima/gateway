-- No markup on requests (docs/platform/adr/0010-admin-controlled-pricing.md, amended
-- 2026-11-27): a request is charged at the provider's cost converted at the effective USD to
-- SAR rate, and the platform's revenue is the fee taken at top-up. The default markup and the
-- per-provider markups go. Dropping the table drops the gateway role's grant on it; the role
-- keeps SELECT on pricing_settings, where it reads the rate and the spread. pricing_change
-- keeps its old rows (append-only history), including those about markups.
DROP TABLE "provider_markup";--> statement-breakpoint
ALTER TABLE "pricing_settings" DROP CONSTRAINT "pricing_settings_default_markup_range";--> statement-breakpoint
ALTER TABLE "pricing_settings" DROP COLUMN "default_markup_bps";
