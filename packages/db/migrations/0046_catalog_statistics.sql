CREATE TABLE "catalog_settings" (
	"id" smallint PRIMARY KEY DEFAULT 1 NOT NULL,
	"min_calls_for_stats" integer DEFAULT 20 NOT NULL,
	"is_traffic_public" boolean DEFAULT false NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_by" text,
	CONSTRAINT "catalog_settings_single_row" CHECK ("catalog_settings"."id" = 1),
	CONSTRAINT "catalog_settings_min_calls_range" CHECK ("catalog_settings"."min_calls_for_stats" between 1 and 1000000)
);
--> statement-breakpoint
CREATE TABLE "catalog_settings_change" (
	"id" bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY (sequence name "catalog_settings_change_id_seq" INCREMENT BY 1 MINVALUE 1 MAXVALUE 9223372036854775807 START WITH 1 CACHE 1),
	"before" jsonb NOT NULL,
	"after" jsonb NOT NULL,
	"changed_by" text NOT NULL,
	"changed_at" timestamp with time zone DEFAULT now() NOT NULL
);
--> statement-breakpoint
CREATE TABLE "endpoint_stat_state" (
	"id" smallint PRIMARY KEY DEFAULT 1 NOT NULL,
	"filled_through" timestamp with time zone,
	"latency_bounds_ms" double precision[],
	"throughput_bounds" double precision[],
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL,
	CONSTRAINT "endpoint_stat_state_single_row" CHECK ("endpoint_stat_state"."id" = 1),
	CONSTRAINT "endpoint_stat_state_bounds" CHECK (("endpoint_stat_state"."latency_bounds_ms" is null and "endpoint_stat_state"."throughput_bounds" is null)
        or (cardinality("endpoint_stat_state"."latency_bounds_ms") = 63
          and cardinality("endpoint_stat_state"."throughput_bounds") = 63))
);
--> statement-breakpoint
CREATE TABLE "model_endpoint_stat_hourly" (
	"hour" timestamp with time zone NOT NULL,
	"provider" text NOT NULL,
	"model" text NOT NULL,
	"provider_key_id" text DEFAULT '' NOT NULL,
	"service_tier" text DEFAULT '' NOT NULL,
	"attempts" bigint DEFAULT 0 NOT NULL,
	"succeeded" bigint DEFAULT 0 NOT NULL,
	"streamed" bigint DEFAULT 0 NOT NULL,
	"prompt_tokens" bigint DEFAULT 0 NOT NULL,
	"completion_tokens" bigint DEFAULT 0 NOT NULL,
	"reasoning_tokens" bigint DEFAULT 0 NOT NULL,
	"cached_read_tokens" bigint DEFAULT 0 NOT NULL,
	"input_cost_usd" numeric(24, 12) DEFAULT 0 NOT NULL,
	"output_cost_usd" numeric(24, 12) DEFAULT 0 NOT NULL,
	"cost_usd" numeric(24, 12) DEFAULT 0 NOT NULL,
	"latency_ms" bigint[] NOT NULL,
	"time_to_first_token_ms" bigint[] NOT NULL,
	"throughput" bigint[] NOT NULL,
	CONSTRAINT "model_endpoint_stat_hourly_hour_provider_model_provider_key_id_service_tier_pk" PRIMARY KEY("hour","provider","model","provider_key_id","service_tier"),
	CONSTRAINT "model_endpoint_stat_hourly_on_the_hour" CHECK (date_trunc('hour', "model_endpoint_stat_hourly"."hour") = "model_endpoint_stat_hourly"."hour"),
	CONSTRAINT "model_endpoint_stat_hourly_counts" CHECK ("model_endpoint_stat_hourly"."attempts" >= 0 and "model_endpoint_stat_hourly"."succeeded" between 0 and "model_endpoint_stat_hourly"."attempts" and "model_endpoint_stat_hourly"."streamed" between 0 and "model_endpoint_stat_hourly"."succeeded"
        and "model_endpoint_stat_hourly"."prompt_tokens" >= 0 and "model_endpoint_stat_hourly"."completion_tokens" >= 0 and "model_endpoint_stat_hourly"."reasoning_tokens" >= 0 and "model_endpoint_stat_hourly"."cached_read_tokens" >= 0
        and "model_endpoint_stat_hourly"."input_cost_usd" >= 0 and "model_endpoint_stat_hourly"."output_cost_usd" >= 0 and "model_endpoint_stat_hourly"."cost_usd" >= 0),
	CONSTRAINT "model_endpoint_stat_hourly_histograms" CHECK (cardinality("model_endpoint_stat_hourly"."latency_ms") = 64
        and cardinality("model_endpoint_stat_hourly"."time_to_first_token_ms") = 64
        and cardinality("model_endpoint_stat_hourly"."throughput") = 64)
);
--> statement-breakpoint
CREATE INDEX "model_endpoint_stat_hourly_model_hour_idx" ON "model_endpoint_stat_hourly" USING btree ("provider","model","hour");--> statement-breakpoint
-- The catalog's figures (docs/platform/plans/model-page.md, SPEC CAT-11, CAT-12). The statistics
-- job fills model_endpoint_stat_hourly from the gateway's log and keeps its place in
-- endpoint_stat_state. A figure needs 20 answered calls to be shown and traffic volume is not
-- public, to start with (owner, 2026-11-28); staff change both in the admin.
INSERT INTO "endpoint_stat_state" ("id") VALUES (1);--> statement-breakpoint
INSERT INTO "catalog_settings" ("id") VALUES (1);--> statement-breakpoint
CREATE OR REPLACE FUNCTION "public"."reject_catalog_settings_change_change"()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'catalog_settings_change is append-only: % is not allowed', TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "catalog_settings_change_append_only_trigger"
BEFORE UPDATE OR DELETE ON "public"."catalog_settings_change"
FOR EACH ROW
EXECUTE FUNCTION "public"."reject_catalog_settings_change_change"();
