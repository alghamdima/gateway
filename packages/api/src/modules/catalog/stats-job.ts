import { and, eq, gte, lt } from "@workspace/db";
import {
  endpointStatState,
  modelEndpointStatHourly,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { getGateway } from "@workspace/gateway-client/server";
import { logger } from "@workspace/shared/logger";

import type {
  GatewayClient,
  GatewayEndpointHour,
} from "@workspace/gateway-client/server";

/**
 * The statistics job (docs/platform/plans/model-page.md, SPEC CAT-11, CAT-12):
 * copies what each endpoint did, hour by hour, from the gateway's log into
 * `model_endpoint_stat_hourly`, so a model page reads a small table instead of
 * the log, and a chart reaches further back than the log is kept. Run every
 * few minutes by /api/internal/endpoint-stats.
 *
 * Each run reads whole hours and replaces them, so running it again changes
 * nothing. It re-reads the last hours as well as the new ones, because the
 * gateway writes a request's row after answering and the hour in progress is
 * not finished. It never replaces an hour it cannot read again: the window
 * starts no earlier than where it stopped last time, unless that is within
 * the hours it always re-reads.
 */

const HOUR_MS = 3_600_000;
/** Hours before the current one that every run reads again. */
const REREAD_HOURS = 3;
/** How far back the first run reads; the gateway refuses a longer window. */
const FIRST_RUN_DAYS = 31;
/** One read from the gateway and one transaction here. */
const CHUNK_HOURS = 24;
const INSERT_BATCH = 500;
const SETTINGS_ID = 1;

export interface EndpointStatsJobResult {
  /** The window read, [from, to), both on the hour. */
  from: string;
  to: string;
  /** Rows written: one per endpoint and hour that had an attempt. */
  rows: number;
}

const startOfHour = (date: Date) =>
  new Date(Math.floor(date.getTime() / HOUR_MS) * HOUR_MS);

const sameBounds = (a: number[], b: number[]) =>
  a.length === b.length &&
  a.every((value, index) => Math.abs(value - (b[index] ?? NaN)) < 1e-9);

const toRow = (hour: GatewayEndpointHour) => ({
  hour: new Date(hour.hour),
  provider: hour.provider,
  model: hour.model,
  providerKeyId: hour.key_id,
  serviceTier: hour.service_tier,
  attempts: hour.attempts,
  succeeded: hour.succeeded,
  streamed: hour.streamed,
  promptTokens: hour.prompt_tokens,
  completionTokens: hour.completion_tokens,
  reasoningTokens: hour.reasoning_tokens,
  cachedReadTokens: hour.cached_read_tokens,
  inputCostUsd: hour.input_cost_usd,
  outputCostUsd: hour.output_cost_usd,
  costUsd: hour.cost_usd,
  latencyMs: hour.latency_ms,
  timeToFirstTokenMs: hour.time_to_first_token_ms,
  throughput: hour.throughput_tokens_per_second,
});

export const runEndpointStatsJob = async (
  gateway: GatewayClient = getGateway(),
  now = new Date(),
): Promise<EndpointStatsJobResult> => {
  const currentHour = startOfHour(now);
  // the hour in progress is read too: its row is replaced by every later run
  const to = new Date(currentHour.getTime() + HOUR_MS);
  const [state] = await db
    .select({ filledThrough: endpointStatState.filledThrough })
    .from(endpointStatState)
    .where(eq(endpointStatState.id, SETTINGS_ID));
  if (!state) {
    // the migration creates the row and nothing deletes it
    throw new Error(
      "endpoint_stat_state has no row: run the database migrations",
    );
  }
  const reread = currentHour.getTime() - REREAD_HOURS * HOUR_MS;
  const earliest = to.getTime() - FIRST_RUN_DAYS * 24 * HOUR_MS;
  const from = new Date(
    Math.max(
      earliest,
      state.filledThrough
        ? Math.min(state.filledThrough.getTime(), reread)
        : earliest,
    ),
  );

  let rows = 0;
  for (
    let start = from.getTime();
    start < to.getTime();
    start += CHUNK_HOURS * HOUR_MS
  ) {
    const chunkFrom = new Date(start);
    const chunkTo = new Date(
      Math.min(start + CHUNK_HOURS * HOUR_MS, to.getTime()),
    );
    rows += await copyHours(gateway, chunkFrom, chunkTo, currentHour);
  }
  if (rows > 0) {
    logger.info(
      `📈 Endpoint statistics: ${rows} endpoint hours from ${from.toISOString()} to ${to.toISOString()}`,
    );
  }
  return { from: from.toISOString(), to: to.toISOString(), rows };
};

/** Replaces the hours of [from, to) with the gateway's answer for them. */
const copyHours = async (
  gateway: GatewayClient,
  from: Date,
  to: Date,
  currentHour: Date,
) => {
  const stats = await gateway.getEndpointStats(from, to);
  return db.transaction(async (tx) => {
    // one run at a time writes: a second waits here and then writes the same hours
    const [state] = await tx
      .select()
      .from(endpointStatState)
      .where(eq(endpointStatState.id, SETTINGS_ID))
      .for("update");
    if (!state) {
      throw new Error("endpoint_stat_state row disappeared during the job");
    }
    const { latency_ms: latency, throughput_tokens_per_second: throughput } =
      stats.bounds;
    if (
      state.latencyBoundsMs &&
      state.throughputBounds &&
      !(
        sameBounds(state.latencyBoundsMs, latency) &&
        sameBounds(state.throughputBounds, throughput)
      )
    ) {
      // every stored histogram is on the stored bounds: another scale cannot be mixed in
      throw new Error(
        "The gateway's histogram bounds differ from the stored ones: the stored statistics must be migrated to the new bounds first",
      );
    }

    await tx
      .delete(modelEndpointStatHourly)
      .where(
        and(
          gte(modelEndpointStatHourly.hour, from),
          lt(modelEndpointStatHourly.hour, to),
        ),
      );
    for (let i = 0; i < stats.hours.length; i += INSERT_BATCH) {
      await tx
        .insert(modelEndpointStatHourly)
        .values(stats.hours.slice(i, i + INSERT_BATCH).map(toRow));
    }

    // only hours that have ended count as filled
    const filled = new Date(Math.min(to.getTime(), currentHour.getTime()));
    await tx
      .update(endpointStatState)
      .set({
        latencyBoundsMs: latency,
        throughputBounds: throughput,
        filledThrough:
          state.filledThrough && state.filledThrough > filled
            ? state.filledThrough
            : filled,
        updatedAt: new Date(),
      })
      .where(eq(endpointStatState.id, SETTINGS_ID));
    return stats.hours.length;
  });
};
