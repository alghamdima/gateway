CREATE TABLE "key_settings" (
	"id" smallint PRIMARY KEY DEFAULT 1 NOT NULL,
	"default_limit_micro_sar" bigint,
	"default_limit_reset" text,
	"default_expiry_seconds" integer DEFAULT 7776000,
	"max_lifetime_seconds" integer,
	"limit_presets_micro_sar" bigint[] DEFAULT '{50000000,200000000,1000000000}'::bigint[] NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_by" text,
	CONSTRAINT "key_settings_single_row" CHECK ("key_settings"."id" = 1),
	CONSTRAINT "key_settings_default_limit_range" CHECK ("key_settings"."default_limit_micro_sar" is null or "key_settings"."default_limit_micro_sar" between 1 and 4000000000000),
	CONSTRAINT "key_settings_default_reset_needs_limit" CHECK ("key_settings"."default_limit_reset" is null or ("key_settings"."default_limit_micro_sar" is not null and "key_settings"."default_limit_reset" in ('daily', 'weekly', 'monthly'))),
	CONSTRAINT "key_settings_default_expiry_value" CHECK ("key_settings"."default_expiry_seconds" is null or "key_settings"."default_expiry_seconds" in (3600, 86400, 604800, 2592000, 7776000, 15552000, 31536000)),
	CONSTRAINT "key_settings_max_lifetime_value" CHECK ("key_settings"."max_lifetime_seconds" is null or "key_settings"."max_lifetime_seconds" in (3600, 86400, 604800, 2592000, 7776000, 15552000, 31536000)),
	CONSTRAINT "key_settings_default_within_max" CHECK ("key_settings"."max_lifetime_seconds" is null or ("key_settings"."default_expiry_seconds" is not null and "key_settings"."default_expiry_seconds" <= "key_settings"."max_lifetime_seconds")),
	CONSTRAINT "key_settings_limit_presets_range" CHECK (cardinality("key_settings"."limit_presets_micro_sar") between 1 and 6 and array_position("key_settings"."limit_presets_micro_sar", null) is null and 0 < all("key_settings"."limit_presets_micro_sar") and 4000000000000 >= all("key_settings"."limit_presets_micro_sar"))
);
--> statement-breakpoint
CREATE TABLE "key_settings_change" (
	"id" bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY (sequence name "key_settings_change_id_seq" INCREMENT BY 1 MINVALUE 1 MAXVALUE 9223372036854775807 START WITH 1 CACHE 1),
	"before" jsonb NOT NULL,
	"after" jsonb NOT NULL,
	"changed_by" text NOT NULL,
	"changed_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
ALTER TABLE "api_key" ADD CONSTRAINT "api_key_expires_after_created" CHECK ("api_key"."expires_at" is null or "api_key"."expires_at" > "api_key"."created_at");--> statement-breakpoint
-- ADMIN-12: what new keys start from. A 90-day default expiry, no default limit and no maximum
-- lifetime: no existing key changes. Staff adjust them in the admin.
INSERT INTO "key_settings" ("id") VALUES (1);--> statement-breakpoint
CREATE OR REPLACE FUNCTION "public"."reject_key_settings_change_change"()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'key_settings_change is append-only: % is not allowed', TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "key_settings_change_append_only_trigger"
BEFORE UPDATE OR DELETE ON "public"."key_settings_change"
FOR EACH ROW
EXECUTE FUNCTION "public"."reject_key_settings_change_change"();--> statement-breakpoint
-- The one definition of a key limit's windows (ADR 0013, KEYS-02): the UTC calendar day, the ISO
-- week from Monday and the calendar month. `at` is a UTC wall-clock time, as the ledger's
-- created_at columns hold, normally now() AT TIME ZONE 'UTC'. A limit without a reset counts all
-- time: its window starts at -infinity and never ends (null). The credits plugin counts a key's
-- spend from key_limit_window_start (plugins/credits/store.go) and says when it resets with
-- key_limit_window_end; the control plane shows usage and resets with the same two functions
-- (packages/api/src/modules/keys/usage.ts). Every role may execute them, gateway_ledger included.
CREATE OR REPLACE FUNCTION "public"."key_limit_window_start"(reset text, at timestamp)
RETURNS timestamp
LANGUAGE sql IMMUTABLE PARALLEL SAFE
AS $$
  SELECT CASE reset
    WHEN 'daily' THEN date_trunc('day', at)
    WHEN 'weekly' THEN date_trunc('week', at)
    WHEN 'monthly' THEN date_trunc('month', at)
    ELSE '-infinity'::timestamp
  END
$$;--> statement-breakpoint
CREATE OR REPLACE FUNCTION "public"."key_limit_window_end"(reset text, at timestamp)
RETURNS timestamp
LANGUAGE sql IMMUTABLE PARALLEL SAFE
AS $$
  SELECT CASE reset
    WHEN 'daily' THEN date_trunc('day', at) + interval '1 day'
    WHEN 'weekly' THEN date_trunc('week', at) + interval '1 week'
    WHEN 'monthly' THEN date_trunc('month', at) + interval '1 month'
  END
$$;
