CREATE TABLE "model_price" (
	"id" bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY (sequence name "model_price_id_seq" INCREMENT BY 1 MINVALUE 1 MAXVALUE 9223372036854775807 START WITH 1 CACHE 1),
	"provider" text NOT NULL,
	"model" text NOT NULL,
	"provider_key_id" text,
	"effective_from" timestamp with time zone NOT NULL,
	"is_removed" boolean DEFAULT false NOT NULL,
	"input_usd" numeric(36, 30),
	"output_usd" numeric(36, 30),
	"cache_read_usd" numeric(36, 30),
	"cache_write_usd" numeric(36, 30),
	"cache_write_1h_usd" numeric(36, 30),
	"other_rates" jsonb DEFAULT '{}'::jsonb NOT NULL,
	"source" text NOT NULL,
	"suggestion_id" bigint,
	"note" text DEFAULT '' NOT NULL,
	"created_by" text NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	CONSTRAINT "model_price_target_effective_from_unique" UNIQUE NULLS NOT DISTINCT("provider","model","provider_key_id","effective_from"),
	CONSTRAINT "model_price_names" CHECK (char_length("model_price"."provider") between 1 and 255 and char_length("model_price"."model") between 1 and 255 and ("model_price"."provider_key_id" is null or char_length("model_price"."provider_key_id") between 1 and 255)),
	CONSTRAINT "model_price_source" CHECK ("model_price"."source" in ('manual', 'suggestion', 'import') and (("model_price"."source" = 'suggestion') = ("model_price"."suggestion_id" is not null))),
	CONSTRAINT "model_price_rates_range" CHECK (("model_price"."input_usd" is null or "model_price"."input_usd" between 0 and 1) and ("model_price"."output_usd" is null or "model_price"."output_usd" between 0 and 1) and ("model_price"."cache_read_usd" is null or "model_price"."cache_read_usd" between 0 and 1) and ("model_price"."cache_write_usd" is null or "model_price"."cache_write_usd" between 0 and 1) and ("model_price"."cache_write_1h_usd" is null or "model_price"."cache_write_1h_usd" between 0 and 1) and jsonb_typeof("model_price"."other_rates") = 'object'
    and not jsonb_path_exists("model_price"."other_rates", '$.keyvalue() ? (!(@.key like_regex "^[a-z][a-z0-9_]{0,99}$"))')
    and not jsonb_path_exists("model_price"."other_rates", '$.* ? (@.type() != "string")')
    and not jsonb_path_exists("model_price"."other_rates", '$.* ? (!(@ like_regex "^[0-9]{1,6}([.][0-9]{1,30})?$"))')
    and not jsonb_path_exists("model_price"."other_rates", '$.* ? (@.double() > 1000)')
    and not ("model_price"."other_rates" ?| array['input_cost_per_token', 'output_cost_per_token', 'cache_read_input_token_cost', 'cache_creation_input_token_cost', 'cache_creation_input_token_cost_above_1hr'])),
	CONSTRAINT "model_price_removal" CHECK (case when "model_price"."is_removed"
          then "model_price"."provider_key_id" is not null and num_nonnulls("model_price"."input_usd", "model_price"."output_usd", "model_price"."cache_read_usd", "model_price"."cache_write_usd", "model_price"."cache_write_1h_usd") = 0 and "model_price"."other_rates" = '{}'::jsonb
          else (num_nonnulls("model_price"."input_usd", "model_price"."output_usd", "model_price"."cache_read_usd", "model_price"."cache_write_usd", "model_price"."cache_write_1h_usd") > 0 or "model_price"."other_rates" <> '{}'::jsonb)
        end),
	CONSTRAINT "model_price_not_backdated" CHECK ("model_price"."effective_from" >= "model_price"."created_at" - interval '5 minutes'),
	CONSTRAINT "model_price_note_length" CHECK (char_length("model_price"."note") <= 500)
);
--> statement-breakpoint
CREATE TABLE "model_price_push" (
	"id" bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY (sequence name "model_price_push_id_seq" INCREMENT BY 1 MINVALUE 1 MAXVALUE 9223372036854775807 START WITH 1 CACHE 1),
	"provider" text NOT NULL,
	"model" text NOT NULL,
	"provider_key_id" text,
	"gateway_override_id" text,
	"pushed_price_id" bigint,
	"pushed_at" timestamp with time zone,
	"attempted_at" timestamp with time zone DEFAULT now() NOT NULL,
	"last_error" text,
	CONSTRAINT "model_price_push_target_unique" UNIQUE NULLS NOT DISTINCT("provider","model","provider_key_id"),
	CONSTRAINT "model_price_push_error_length" CHECK ("model_price_push"."last_error" is null or char_length("model_price_push"."last_error") <= 2000)
);
--> statement-breakpoint
CREATE TABLE "model_price_suggestion" (
	"id" bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY (sequence name "model_price_suggestion_id_seq" INCREMENT BY 1 MINVALUE 1 MAXVALUE 9223372036854775807 START WITH 1 CACHE 1),
	"provider" text NOT NULL,
	"model" text NOT NULL,
	"source" text NOT NULL,
	"base_price_id" bigint,
	"input_usd" numeric(36, 30),
	"output_usd" numeric(36, 30),
	"cache_read_usd" numeric(36, 30),
	"cache_write_usd" numeric(36, 30),
	"cache_write_1h_usd" numeric(36, 30),
	"other_rates" jsonb DEFAULT '{}'::jsonb NOT NULL,
	"status" text DEFAULT 'pending' NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"decided_by" text,
	"decided_at" timestamp with time zone,
	"reason" text DEFAULT '' NOT NULL,
	CONSTRAINT "model_price_suggestion_names" CHECK (char_length("model_price_suggestion"."provider") between 1 and 255 and char_length("model_price_suggestion"."model") between 1 and 255),
	CONSTRAINT "model_price_suggestion_status" CHECK ("model_price_suggestion"."status" in ('pending', 'approved', 'rejected', 'superseded')),
	CONSTRAINT "model_price_suggestion_source" CHECK ("model_price_suggestion"."source" in ('datasheet')),
	CONSTRAINT "model_price_suggestion_rates" CHECK (("model_price_suggestion"."input_usd" is null or "model_price_suggestion"."input_usd" between 0 and 1) and ("model_price_suggestion"."output_usd" is null or "model_price_suggestion"."output_usd" between 0 and 1) and ("model_price_suggestion"."cache_read_usd" is null or "model_price_suggestion"."cache_read_usd" between 0 and 1) and ("model_price_suggestion"."cache_write_usd" is null or "model_price_suggestion"."cache_write_usd" between 0 and 1) and ("model_price_suggestion"."cache_write_1h_usd" is null or "model_price_suggestion"."cache_write_1h_usd" between 0 and 1) and jsonb_typeof("model_price_suggestion"."other_rates") = 'object'
    and not jsonb_path_exists("model_price_suggestion"."other_rates", '$.keyvalue() ? (!(@.key like_regex "^[a-z][a-z0-9_]{0,99}$"))')
    and not jsonb_path_exists("model_price_suggestion"."other_rates", '$.* ? (@.type() != "string")')
    and not jsonb_path_exists("model_price_suggestion"."other_rates", '$.* ? (!(@ like_regex "^[0-9]{1,6}([.][0-9]{1,30})?$"))')
    and not jsonb_path_exists("model_price_suggestion"."other_rates", '$.* ? (@.double() > 1000)')
    and not ("model_price_suggestion"."other_rates" ?| array['input_cost_per_token', 'output_cost_per_token', 'cache_read_input_token_cost', 'cache_creation_input_token_cost', 'cache_creation_input_token_cost_above_1hr']) and (num_nonnulls("model_price_suggestion"."input_usd", "model_price_suggestion"."output_usd", "model_price_suggestion"."cache_read_usd", "model_price_suggestion"."cache_write_usd", "model_price_suggestion"."cache_write_1h_usd") > 0 or "model_price_suggestion"."other_rates" <> '{}'::jsonb)),
	CONSTRAINT "model_price_suggestion_decision" CHECK (case "model_price_suggestion"."status"
          when 'pending' then "model_price_suggestion"."decided_at" is null and "model_price_suggestion"."decided_by" is null
          when 'superseded' then "model_price_suggestion"."decided_at" is not null
          else "model_price_suggestion"."decided_at" is not null and "model_price_suggestion"."decided_by" is not null
        end and ("model_price_suggestion"."status" <> 'rejected' or char_length("model_price_suggestion"."reason") > 0)),
	CONSTRAINT "model_price_suggestion_reason_length" CHECK (char_length("model_price_suggestion"."reason") <= 500)
);
--> statement-breakpoint
ALTER TABLE "model_price" ADD CONSTRAINT "model_price_suggestion_id_model_price_suggestion_id_fk" FOREIGN KEY ("suggestion_id") REFERENCES "public"."model_price_suggestion"("id") ON DELETE restrict ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "model_price_push" ADD CONSTRAINT "model_price_push_pushed_price_id_model_price_id_fk" FOREIGN KEY ("pushed_price_id") REFERENCES "public"."model_price"("id") ON DELETE restrict ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "model_price_suggestion" ADD CONSTRAINT "model_price_suggestion_base_price_id_model_price_id_fk" FOREIGN KEY ("base_price_id") REFERENCES "public"."model_price"("id") ON DELETE restrict ON UPDATE no action;--> statement-breakpoint
CREATE INDEX "model_price_provider_key_idx" ON "model_price" USING btree ("provider_key_id") WHERE "model_price"."provider_key_id" is not null;--> statement-breakpoint
CREATE INDEX "model_price_suggestion_idx" ON "model_price" USING btree ("suggestion_id") WHERE "model_price"."suggestion_id" is not null;--> statement-breakpoint
CREATE INDEX "model_price_push_pushed_price_idx" ON "model_price_push" USING btree ("pushed_price_id") WHERE "model_price_push"."pushed_price_id" is not null;--> statement-breakpoint
CREATE UNIQUE INDEX "model_price_suggestion_pending_unique" ON "model_price_suggestion" USING btree ("provider","model") WHERE "model_price_suggestion"."status" = 'pending';--> statement-breakpoint
CREATE INDEX "model_price_suggestion_status_idx" ON "model_price_suggestion" USING btree ("status","created_at");--> statement-breakpoint
CREATE INDEX "model_price_suggestion_base_price_idx" ON "model_price_suggestion" USING btree ("base_price_id") WHERE "model_price_suggestion"."base_price_id" is not null;--> statement-breakpoint
CREATE OR REPLACE FUNCTION "public"."reject_model_price_change"()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'model_price is append-only: % is not allowed (a new version supersedes a price)', TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;--> statement-breakpoint
CREATE TRIGGER "model_price_append_only_trigger"
BEFORE UPDATE OR DELETE ON "public"."model_price"
FOR EACH ROW
EXECUTE FUNCTION "public"."reject_model_price_change"();
