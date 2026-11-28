import {
  bigint,
  boolean,
  check,
  index,
  jsonb,
  numeric,
  pgTable,
  text,
  timestamp,
  unique,
  uniqueIndex,
} from "drizzle-orm/pg-core";
import { sql } from "drizzle-orm/sql";

import { createSelectSchema } from "../lib/zod";

import type { AnyPgColumn } from "drizzle-orm/pg-core";
import type * as z from "zod";

/**
 * Our own model price table (ADMIN-09, docs/platform/adr/0018-model-price-table.md).
 * Prices are USD per unit, as providers price; customers see them in SAR at
 * the effective rate (ADR 0017). A price is never edited: a new version with a
 * later `effective_from` supersedes it. The approved version in force is pushed
 * to the gateway's pricing overrides, which is what the gateway charges with.
 * The gateway reads none of these tables.
 */

/** Digits after the point a rate may have: Postgres would round more, so the API refuses them. */
export const MODEL_PRICE_SCALE = 30;
/** USD per token: a ceiling against a typo, not a business rule (the datasheet's highest is 0.36). */
export const MAX_TOKEN_RATE_USD = 1;
/** USD per any other unit (image, second, character, request): a ceiling against a typo. */
export const MAX_UNIT_RATE_USD = 1000;
/** How far back an effective time may be set: history already charged is not rewritten. */
export const MODEL_PRICE_BACKDATE_TOLERANCE_MINUTES = 5;
/** The longest note or rejection reason staff may write. */
export const MAX_MODEL_PRICE_NOTE_LENGTH = 500;
/** The longest provider or model name the gateway is expected to use. */
export const MAX_MODEL_NAME_LENGTH = 255;

/** Where a version came from. */
export const MODEL_PRICE_SOURCES = ["manual", "suggestion", "import"] as const;
export type ModelPriceSource = (typeof MODEL_PRICE_SOURCES)[number];

export const MODEL_PRICE_SUGGESTION_STATUSES = [
  "pending",
  "approved",
  "rejected",
  "superseded",
] as const;
export type ModelPriceSuggestionStatus =
  (typeof MODEL_PRICE_SUGGESTION_STATUSES)[number];

/** What proposed a suggestion. */
export const MODEL_PRICE_SUGGESTION_SOURCES = ["datasheet"] as const;

/**
 * The five common rates, typed, with the gateway's (datasheet's) name for
 * each: what the push sends and what the import reads.
 */
export const TYPED_RATE_FIELDS = {
  inputUsd: "input_cost_per_token",
  outputUsd: "output_cost_per_token",
  cacheReadUsd: "cache_read_input_token_cost",
  cacheWriteUsd: "cache_creation_input_token_cost",
  cacheWrite1hUsd: "cache_creation_input_token_cost_above_1hr",
} as const;
export type TypedRateColumn = keyof typeof TYPED_RATE_FIELDS;

/** Every other unit: `{ "<gateway field>": "<decimal>" }`, keys allowlisted by the API. */
export type OtherRates = Record<string, string>;

const RATE = {
  precision: 36,
  scale: MODEL_PRICE_SCALE,
  mode: "string",
} as const;
const rate = (name?: string) => (name ? numeric(name, RATE) : numeric(RATE));
const at = () => timestamp({ withTimezone: true, mode: "date" });

const rateColumns = () => ({
  /** USD per input token. */
  inputUsd: rate(),
  /** USD per output token. */
  outputUsd: rate(),
  /** USD per cached input token read. */
  cacheReadUsd: rate(),
  /** USD per input token written to the cache: the default (5-minute) rate. */
  cacheWriteUsd: rate(),
  /** USD per input token written to the 1-hour cache, where the provider has one. */
  cacheWrite1hUsd: rate("cache_write_1h_usd"),
  otherRates: jsonb().$type<OtherRates>().notNull().default({}),
});

const typedRateKeys = sql.raw(
  `array[${Object.values(TYPED_RATE_FIELDS)
    .map((field) => `'${field}'`)
    .join(", ")}]`,
);

/** Each typed rate is null or between 0 and the token ceiling. */
const typedRatesInRange = (columns: AnyPgColumn[]) =>
  sql.join(
    columns.map(
      (column) =>
        sql`(${column} is null or ${column} between 0 and ${sql.raw(String(MAX_TOKEN_RATE_USD))})`,
    ),
    sql` and `,
  );

/**
 * other_rates is an object of snake_case names (none of the typed five) to
 * decimal strings between 0 and the unit ceiling. Which names are allowed is
 * the API's (rates.ts): it follows the gateway's `Options` without a migration.
 */
const otherRatesShape = (otherRates: AnyPgColumn) =>
  sql`jsonb_typeof(${otherRates}) = 'object'
    and not jsonb_path_exists(${otherRates}, '$.keyvalue() ? (!(@.key like_regex "^[a-z][a-z0-9_]{0,99}$"))')
    and not jsonb_path_exists(${otherRates}, '$.* ? (@.type() != "string")')
    and not jsonb_path_exists(${otherRates}, '$.* ? (!(@ like_regex "^[0-9]{1,6}([.][0-9]{1,${sql.raw(String(MODEL_PRICE_SCALE))}})?$"))')
    and not jsonb_path_exists(${otherRates}, '$.* ? (@.double() > ${sql.raw(String(MAX_UNIT_RATE_USD))})')
    and not (${otherRates} ?| ${typedRateKeys})`;

/** At least one rate is set: a price with no rate would charge nothing. */
const hasSomeRate = (columns: AnyPgColumn[], otherRates: AnyPgColumn) =>
  sql`(num_nonnulls(${sql.join(columns, sql`, `)}) > 0 or ${otherRates} <> '{}'::jsonb)`;

const nameLength = (column: AnyPgColumn) =>
  sql`char_length(${column}) between 1 and ${sql.raw(String(MAX_MODEL_NAME_LENGTH))}`;

/**
 * One version of the price of a model at one provider, or at one provider key
 * (a negotiated rate). Append-only: a trigger rejects UPDATE and DELETE. The
 * price at time T is the row with the greatest `effective_from <= T` for the
 * target; the unique constraint makes it unique, so two prices are never in
 * force at once (intervals come from the next version and cannot overlap).
 */
export const modelPrice = pgTable(
  "model_price",
  {
    id: bigint({ mode: "number" }).primaryKey().generatedAlwaysAsIdentity(),
    /** The gateway's provider name. */
    provider: text().notNull(),
    /** The model as the gateway's catalog names it. */
    model: text().notNull(),
    /** The gateway's key_id for a negotiated price; null is the provider-wide price. */
    providerKeyId: text(),
    /** In force from here until the target's next version. */
    effectiveFrom: at().notNull(),
    /** A key's own price ends here; the provider-wide price applies again. */
    isRemoved: boolean().notNull().default(false),
    ...rateColumns(),
    source: text({ enum: MODEL_PRICE_SOURCES }).notNull(),
    /** The suggestion approved into this version (source "suggestion"). */
    suggestionId: bigint({ mode: "number" }).references(
      (): AnyPgColumn => modelPriceSuggestion.id,
      { onDelete: "restrict" },
    ),
    note: text().notNull().default(""),
    createdBy: text().notNull(),
    createdAt: at().notNull().defaultNow(),
  },
  (t) => {
    const rates = [
      t.inputUsd,
      t.outputUsd,
      t.cacheReadUsd,
      t.cacheWriteUsd,
      t.cacheWrite1hUsd,
    ];
    return [
      // serves "price at T" (backward scan, LIMIT 1), "current price of every
      // target" (DISTINCT ON) and the history of one model (prefix range scan)
      unique("model_price_target_effective_from_unique")
        .on(t.provider, t.model, t.providerKeyId, t.effectiveFrom)
        .nullsNotDistinct(),
      index("model_price_provider_key_idx")
        .on(t.providerKeyId)
        .where(sql`${t.providerKeyId} is not null`),
      index("model_price_suggestion_idx")
        .on(t.suggestionId)
        .where(sql`${t.suggestionId} is not null`),
      check(
        "model_price_names",
        sql`${nameLength(t.provider)} and ${nameLength(t.model)} and (${t.providerKeyId} is null or ${nameLength(t.providerKeyId)})`,
      ),
      check(
        "model_price_source",
        sql`${t.source} in ('manual', 'suggestion', 'import') and ((${t.source} = 'suggestion') = (${t.suggestionId} is not null))`,
      ),
      check(
        "model_price_rates_range",
        sql`${typedRatesInRange(rates)} and ${otherRatesShape(t.otherRates)}`,
      ),
      // a removal names a key and carries no rate; a price carries at least one
      check(
        "model_price_removal",
        sql`case when ${t.isRemoved}
          then ${t.providerKeyId} is not null and num_nonnulls(${sql.join(rates, sql`, `)}) = 0 and ${t.otherRates} = '{}'::jsonb
          else ${hasSomeRate(rates, t.otherRates)}
        end`,
      ),
      // history already charged cannot be rewritten; the future is allowed
      check(
        "model_price_not_backdated",
        sql`${t.effectiveFrom} >= ${t.createdAt} - interval '${sql.raw(String(MODEL_PRICE_BACKDATE_TOLERANCE_MINUTES))} minutes'`,
      ),
      check(
        "model_price_note_length",
        sql`char_length(${t.note}) <= ${sql.raw(String(MAX_MODEL_PRICE_NOTE_LENGTH))}`,
      ),
    ];
  },
);

/**
 * A provider-wide price a source proposes (the gateway's datasheet). A sync
 * only ever writes here, never to model_price: a price staff set always wins.
 * Approving one inserts a model_price version; rejecting leaves the price as it is.
 */
export const modelPriceSuggestion = pgTable(
  "model_price_suggestion",
  {
    id: bigint({ mode: "number" }).primaryKey().generatedAlwaysAsIdentity(),
    provider: text().notNull(),
    model: text().notNull(),
    source: text({ enum: MODEL_PRICE_SUGGESTION_SOURCES }).notNull(),
    /** The provider-wide version in force when it was made; null for a model with no price yet. */
    basePriceId: bigint({ mode: "number" }).references(
      (): AnyPgColumn => modelPrice.id,
      { onDelete: "restrict" },
    ),
    ...rateColumns(),
    status: text({ enum: MODEL_PRICE_SUGGESTION_STATUSES })
      .notNull()
      .default("pending"),
    createdAt: at().notNull().defaultNow(),
    /** Staff who approved or rejected it; null while pending, or superseded by a newer suggestion. */
    decidedBy: text(),
    decidedAt: at(),
    /** Why it was rejected (required) or approved (optional). */
    reason: text().notNull().default(""),
  },
  (t) => {
    const rates = [
      t.inputUsd,
      t.outputUsd,
      t.cacheReadUsd,
      t.cacheWriteUsd,
      t.cacheWrite1hUsd,
    ];
    return [
      // one open suggestion per model
      uniqueIndex("model_price_suggestion_pending_unique")
        .on(t.provider, t.model)
        .where(sql`${t.status} = 'pending'`),
      index("model_price_suggestion_status_idx").on(t.status, t.createdAt),
      index("model_price_suggestion_base_price_idx")
        .on(t.basePriceId)
        .where(sql`${t.basePriceId} is not null`),
      check(
        "model_price_suggestion_names",
        sql`${nameLength(t.provider)} and ${nameLength(t.model)}`,
      ),
      check(
        "model_price_suggestion_status",
        sql`${t.status} in ('pending', 'approved', 'rejected', 'superseded')`,
      ),
      check("model_price_suggestion_source", sql`${t.source} in ('datasheet')`),
      check(
        "model_price_suggestion_rates",
        sql`${typedRatesInRange(rates)} and ${otherRatesShape(t.otherRates)} and ${hasSomeRate(rates, t.otherRates)}`,
      ),
      // pending: undecided; approved or rejected: by staff, at a time; superseded: at a time
      check(
        "model_price_suggestion_decision",
        sql`case ${t.status}
          when 'pending' then ${t.decidedAt} is null and ${t.decidedBy} is null
          when 'superseded' then ${t.decidedAt} is not null
          else ${t.decidedAt} is not null and ${t.decidedBy} is not null
        end and (${t.status} <> 'rejected' or char_length(${t.reason}) > 0)`,
      ),
      check(
        "model_price_suggestion_reason_length",
        sql`char_length(${t.reason}) <= ${sql.raw(String(MAX_MODEL_PRICE_NOTE_LENGTH))}`,
      ),
    ];
  },
);

/**
 * What the gateway holds for each target: the override that carries its price
 * and the version last pushed. Operational state, kept apart from the
 * immutable prices; the push and the reconciliation keep it current.
 */
export const modelPricePush = pgTable(
  "model_price_push",
  {
    id: bigint({ mode: "number" }).primaryKey().generatedAlwaysAsIdentity(),
    provider: text().notNull(),
    model: text().notNull(),
    providerKeyId: text(),
    /** The pricing override's id in the gateway; null once it was deleted. */
    gatewayOverrideId: text(),
    /** The version the gateway carries; null after a key's price was removed. */
    pushedPriceId: bigint({ mode: "number" }).references(() => modelPrice.id, {
      onDelete: "restrict",
    }),
    pushedAt: at(),
    attemptedAt: at().notNull().defaultNow(),
    /** The last push's failure, cleared by the next success. */
    lastError: text(),
  },
  (t) => [
    unique("model_price_push_target_unique")
      .on(t.provider, t.model, t.providerKeyId)
      .nullsNotDistinct(),
    index("model_price_push_pushed_price_idx")
      .on(t.pushedPriceId)
      .where(sql`${t.pushedPriceId} is not null`),
    check(
      "model_price_push_error_length",
      sql`${t.lastError} is null or char_length(${t.lastError}) <= 2000`,
    ),
  ],
);

export const selectModelPriceSchema = createSelectSchema(modelPrice);
export const selectModelPriceSuggestionSchema =
  createSelectSchema(modelPriceSuggestion);

export type SelectModelPrice = z.infer<typeof selectModelPriceSchema>;
export type SelectModelPriceSuggestion = z.infer<
  typeof selectModelPriceSuggestionSchema
>;
