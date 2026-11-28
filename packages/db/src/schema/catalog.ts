import { sql } from "drizzle-orm";
import {
  bigint,
  boolean,
  check,
  doublePrecision,
  index,
  integer,
  jsonb,
  numeric,
  pgTable,
  primaryKey,
  smallint,
  text,
  timestamp,
} from "drizzle-orm/pg-core";

/**
 * The model catalog's own data (docs/platform/plans/model-page.md, SPEC CAT-11
 * and CAT-12): what each endpoint did, hour by hour, and what staff set about
 * showing it.
 *
 * An endpoint is one way a model is served: the provider, the model as that
 * provider names it, the provider key that carried the call (its region and
 * data terms are in `provider_endpoint`, its price in `model_price`) and the
 * service tier the provider served it at. It is not a table of its own: it is
 * that tuple, and a model has as many endpoints as tuples that serve it.
 */

/** How many buckets each timing histogram has; the gateway's EndpointStatsBuckets. */
export const ENDPOINT_STAT_BUCKETS = 64;

const at = () => timestamp({ withTimezone: true, mode: "date" });
const count = () => bigint({ mode: "number" }).notNull().default(0);
const usd = () =>
  numeric({ precision: 24, scale: 12, mode: "number" }).notNull().default(0);
const histogram = () => bigint({ mode: "number" }).array().notNull();

/**
 * One endpoint's attempts in one UTC hour, as the gateway counted them from
 * its log (`GET /api/logs/endpoints/hourly`). Filled by the statistics job
 * (`modules/catalog/stats-job.ts`), which rewrites whole hours, so a row is
 * always the gateway's answer for that hour and running the job again changes
 * nothing. The gateway's log is kept for a limited time; these rows are kept,
 * so a chart reaches further back than the log.
 *
 * The three timings are histograms, not percentiles: the counts of two hours
 * or two endpoints add up, percentiles do not. Their bucket bounds are in
 * `endpoint_stat_state`.
 */
export const modelEndpointStatHourly = pgTable(
  "model_endpoint_stat_hourly",
  {
    /** The start of the hour, UTC. */
    hour: at().notNull(),
    /** The gateway's provider name. */
    provider: text().notNull(),
    /** The model as the gateway's catalog names it. */
    model: text().notNull(),
    /** The gateway's key_id of the provider key; '' for a keyless provider. */
    providerKeyId: text().notNull().default(""),
    /** As the provider reported it; '' when it reported none. */
    serviceTier: text().notNull().default(""),
    /** Calls that reached the provider and ended. */
    attempts: count(),
    /** Of which answered. */
    succeeded: count(),
    /** Of the answered, how many were streams. */
    streamed: count(),
    promptTokens: count(),
    completionTokens: count(),
    /** Part of the completion tokens. */
    reasoningTokens: count(),
    /** Part of the prompt tokens. */
    cachedReadTokens: count(),
    /** What the answered calls cost at the provider, USD, split as the catalog priced them. */
    inputCostUsd: usd(),
    outputCostUsd: usd(),
    costUsd: usd(),
    /** Request received to response finished, answered calls. */
    latencyMs: histogram(),
    /** Streams only. */
    timeToFirstTokenMs: histogram(),
    /** Completion tokens per second of the provider's own time. */
    throughput: histogram(),
  },
  (t) => [
    primaryKey({
      columns: [t.hour, t.provider, t.model, t.providerKeyId, t.serviceTier],
    }),
    // a model's page reads its endpoints over a window
    index("model_endpoint_stat_hourly_model_hour_idx").on(
      t.provider,
      t.model,
      t.hour,
    ),
    check(
      "model_endpoint_stat_hourly_on_the_hour",
      sql`date_trunc('hour', ${t.hour}) = ${t.hour}`,
    ),
    check(
      "model_endpoint_stat_hourly_counts",
      sql`${t.attempts} >= 0 and ${t.succeeded} between 0 and ${t.attempts} and ${t.streamed} between 0 and ${t.succeeded}
        and ${t.promptTokens} >= 0 and ${t.completionTokens} >= 0 and ${t.reasoningTokens} >= 0 and ${t.cachedReadTokens} >= 0
        and ${t.inputCostUsd} >= 0 and ${t.outputCostUsd} >= 0 and ${t.costUsd} >= 0`,
    ),
    check(
      "model_endpoint_stat_hourly_histograms",
      sql`cardinality(${t.latencyMs}) = ${sql.raw(String(ENDPOINT_STAT_BUCKETS))}
        and cardinality(${t.timeToFirstTokenMs}) = ${sql.raw(String(ENDPOINT_STAT_BUCKETS))}
        and cardinality(${t.throughput}) = ${sql.raw(String(ENDPOINT_STAT_BUCKETS))}`,
    ),
  ],
);

/**
 * Where the statistics job stands. A single row (id = 1), seeded by migration
 * 0046.
 *
 * - filledThrough: every hour before it has been read from the gateway at
 *   least once after it ended. Null: the job has not run.
 * - The bucket bounds the gateway answered with (upper bounds, one fewer than
 *   buckets; the last bucket has none). They are the meaning of every
 *   histogram stored: if the gateway ever answers with other bounds the job
 *   stops instead of mixing two scales in one table.
 */
export const endpointStatState = pgTable(
  "endpoint_stat_state",
  {
    id: smallint().primaryKey().default(1),
    filledThrough: at(),
    latencyBoundsMs: doublePrecision().array(),
    throughputBounds: doublePrecision().array(),
    updatedAt: at().notNull().defaultNow(),
  },
  (t) => [
    check("endpoint_stat_state_single_row", sql`${t.id} = 1`),
    check(
      "endpoint_stat_state_bounds",
      sql`(${t.latencyBoundsMs} is null and ${t.throughputBounds} is null)
        or (cardinality(${t.latencyBoundsMs}) = ${sql.raw(String(ENDPOINT_STAT_BUCKETS - 1))}
          and cardinality(${t.throughputBounds}) = ${sql.raw(String(ENDPOINT_STAT_BUCKETS - 1))})`,
    ),
  ],
);

/** A ceiling against a typo on the least number of answered calls a figure needs. */
export const MAX_MIN_CALLS_FOR_STATS = 1_000_000;

/**
 * What staff set about the catalog's figures (SPEC rule 6). A single row
 * (id = 1), seeded by migration 0046; each change is recorded in
 * catalog_settings_change.
 *
 * - minCallsForStats: a performance or price figure is shown only when at
 *   least this many answered calls are behind it; below it the page says
 *   there is not enough data, as a figure from three calls would mislead.
 * - isTrafficPublic: whether a model's token volume and each endpoint's
 *   share of it are shown to visitors. Off to start with (owner, 2026-11-28).
 */
export const catalogSettings = pgTable(
  "catalog_settings",
  {
    id: smallint().primaryKey().default(1),
    minCallsForStats: integer().notNull().default(20),
    isTrafficPublic: boolean().notNull().default(false),
    updatedAt: at().notNull().defaultNow(),
    updatedBy: text(),
  },
  (t) => [
    check("catalog_settings_single_row", sql`${t.id} = 1`),
    check(
      "catalog_settings_min_calls_range",
      sql`${t.minCallsForStats} between 1 and ${sql.raw(String(MAX_MIN_CALLS_FOR_STATS))}`,
    ),
  ],
);

/** Append-only: a trigger rejects UPDATE and DELETE, as for key_settings_change. */
export const catalogSettingsChange = pgTable("catalog_settings_change", {
  id: bigint({ mode: "number" }).primaryKey().generatedAlwaysAsIdentity(),
  before: jsonb().$type<Record<string, unknown>>().notNull(),
  after: jsonb().$type<Record<string, unknown>>().notNull(),
  changedBy: text().notNull(),
  changedAt: at().notNull().defaultNow(),
});

export type SelectModelEndpointStatHourly =
  typeof modelEndpointStatHourly.$inferSelect;
