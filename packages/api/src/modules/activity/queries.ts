import { and, eq, gte, inArray, sql } from "@workspace/db";
import {
  apiKey,
  creditLedgerEntry,
  gatewayCustomer,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import {
  GatewayError,
  GatewayNotConfiguredError,
  getGateway,
} from "@workspace/gateway-client/server";
import { logger } from "@workspace/shared/logger";

import type {
  GatewayClient,
  GatewayGenerationSummary,
  GatewayLatencySummary,
} from "@workspace/gateway-client/server";

/**
 * The activity page (step 9.1, SPEC ACT-01/02/04): spend, requests, tokens and
 * timings over a window, by day, by model and by key.
 *
 * Two sources, each for what it is right about:
 * - Money and tokens come from the ledger's usage entries, kept for good (ADR
 *   0013, 0014). The ledger holds charged attempts only: a free or failed
 *   attempt costs nothing and writes no entry (CHARGE-06).
 * - Requests and timings come from the gateway's request log, through its
 *   generation summary: a request is counted once however many attempts
 *   (fallbacks) it took, free and failed ones included, as OpenRouter counts
 *   generations. A request refused before any provider was called (a limit,
 *   the data policy) is not a generation: it is counted apart, by reason. The
 *   log is kept for its retention window only, so older days show no requests.
 *
 * Amounts are micro-SAR. When the log cannot be read, `requests` is null and
 * the money and tokens still answer.
 */

export interface ActivityFigures {
  spend: number;
  /** Generations; null when the gateway's log could not be read. */
  requests: number | null;
  promptTokens: number;
  completionTokens: number;
  /** The part of the completion spent reasoning. */
  reasoningTokens: number;
  /** The part of the prompt read from the provider's cache. */
  cachedTokens: number;
  /** The part of the prompt written to the provider's cache. */
  cacheWriteTokens: number;
}

export interface ActivityRefusal {
  limitSource: string;
  reason: string;
  statusCode: number;
  requests: number;
}

export interface ActivityTimings {
  /** Successful generations the figures cover. */
  measured: number;
  streamed: number;
  /** Milliseconds; streams to their first chunk, other requests their whole latency. */
  timeToFirstToken: GatewayLatencySummary;
  /** Milliseconds, request received to response finished. */
  latency: GatewayLatencySummary;
  /** Milliseconds the provider took, on average. */
  generationTimeAvg: number;
  /** Completion tokens per second of generation time, on average. */
  throughputAvg: number;
}

export interface Activity {
  from: string;
  to: string;
  totals: ActivityFigures & {
    /** Generations no attempt of which succeeded; null without the log. */
    failed: number | null;
    /** Requests refused before any provider was called; null without the log. */
    refused: number | null;
    /**
     * Cached prompt tokens over the prompt tokens of the entries that report
     * caching (written since migration 0041); null when none do.
     */
    cacheHitRate: number | null;
  };
  byDay: (ActivityFigures & { day: string; refused: number | null })[];
  byModel: (ActivityFigures & { model: string; provider: string })[];
  byKey: (ActivityFigures & {
    keyId: string | null;
    name: string | null;
    prefix: string | null;
  })[];
  refusals: ActivityRefusal[];
  timings: ActivityTimings | null;
  /** False when the gateway's log could not be read: no requests, refusals or timings. */
  requestLogAvailable: boolean;
}

const DAY_MS = 86_400_000;

const tokenSum = (column: unknown) => sql<string>`coalesce(sum(${column}), 0)`;
const metadataTokens = (key: string) =>
  sql<string>`coalesce(sum((${creditLedgerEntry.metadata}->>${key})::bigint), 0)`;

const ledgerFigures = {
  spend: sql<string>`coalesce(-sum(${creditLedgerEntry.amount}), 0)`,
  promptTokens: metadataTokens("prompt_tokens"),
  completionTokens: metadataTokens("completion_tokens"),
  reasoningTokens: tokenSum(creditLedgerEntry.reasoningTokens),
  cachedTokens: tokenSum(creditLedgerEntry.cachedTokens),
  cacheWriteTokens: tokenSum(creditLedgerEntry.cacheWriteTokens),
};

type LedgerRow = Record<keyof typeof ledgerFigures, string>;

const toFigures = (
  row: LedgerRow | undefined,
  requests: number | null,
): ActivityFigures => ({
  spend: Number(row?.spend ?? 0),
  requests,
  promptTokens: Number(row?.promptTokens ?? 0),
  completionTokens: Number(row?.completionTokens ?? 0),
  reasoningTokens: Number(row?.reasoningTokens ?? 0),
  cachedTokens: Number(row?.cachedTokens ?? 0),
  cacheWriteTokens: Number(row?.cacheWriteTokens ?? 0),
});

/** The last `days` UTC days, today included. */
export const getActivity = async ({
  organizationId,
  days,
  now = new Date(),
  gateway,
}: {
  organizationId: string;
  days: number;
  now?: Date;
  gateway?: GatewayClient;
}): Promise<Activity> => {
  const today = Date.UTC(
    now.getUTCFullYear(),
    now.getUTCMonth(),
    now.getUTCDate(),
  );
  const from = new Date(today - (days - 1) * DAY_MS);
  const usage = and(
    eq(creditLedgerEntry.organizationId, organizationId),
    eq(creditLedgerEntry.kind, "usage"),
    gte(creditLedgerEntry.createdAt, from),
  );
  const day = sql<string>`to_char(date_trunc('day', ${creditLedgerEntry.createdAt}), 'YYYY-MM-DD')`;
  const model = sql<string>`coalesce(${creditLedgerEntry.metadata}->>'model', '')`;
  const provider = sql<string>`coalesce(${creditLedgerEntry.metadata}->>'provider', '')`;

  const [totals, cacheable, daily, models, keys, summary] = await Promise.all([
    db.select(ledgerFigures).from(creditLedgerEntry).where(usage),
    // the hit rate counts only entries that say how much of their prompt was cached
    db
      .select({ promptTokens: metadataTokens("prompt_tokens") })
      .from(creditLedgerEntry)
      .where(and(usage, sql`${creditLedgerEntry.cachedTokens} is not null`)),
    db
      .select({ day, ...ledgerFigures })
      .from(creditLedgerEntry)
      .where(usage)
      .groupBy(day),
    db
      .select({ model, provider, ...ledgerFigures })
      .from(creditLedgerEntry)
      .where(usage)
      .groupBy(model, provider),
    db
      .select({
        virtualKeyId: creditLedgerEntry.virtualKeyId,
        ...ledgerFigures,
      })
      .from(creditLedgerEntry)
      .where(usage)
      .groupBy(creditLedgerEntry.virtualKeyId),
    organizationSummary(organizationId, from, now, gateway),
  ]);
  const available = summary !== undefined;
  const generations = (n: number | undefined) => (available ? (n ?? 0) : null);

  // every day of the window, zero where nothing ran, so a chart has no gaps
  const ledgerByDay = new Map(daily.map((row) => [row.day, row]));
  const logByDay = new Map((summary?.by_day ?? []).map((d) => [d.day, d]));
  const byDay = Array.from({ length: days }, (_, i) => {
    const date = new Date(from.getTime() + i * DAY_MS)
      .toISOString()
      .slice(0, 10);
    const log = logByDay.get(date);
    return {
      day: date,
      ...toFigures(ledgerByDay.get(date), generations(log?.generations)),
      refused: generations(log?.refused),
    };
  });

  // a model can be in the log only (free, or every attempt failed) or the ledger only (log expired)
  const byModel = new Map<string, Activity["byModel"][number]>();
  for (const row of models) {
    byModel.set(modelKey(row.provider, row.model), {
      model: row.model,
      provider: row.provider,
      ...toFigures(row, generations(0)),
    });
  }
  for (const row of summary?.by_model ?? []) {
    const key = modelKey(row.provider, row.model);
    const known = byModel.get(key);
    if (known) {
      known.requests = row.generations;
    } else {
      byModel.set(key, {
        model: row.model,
        provider: row.provider,
        ...toFigures(undefined, row.generations),
      });
    }
  }

  return {
    from: from.toISOString(),
    to: now.toISOString(),
    totals: {
      ...toFigures(
        totals[0],
        generations(sumOf(summary?.by_day, (d) => d.generations)),
      ),
      failed: generations(sumOf(summary?.by_day, (d) => d.failed)),
      refused: generations(sumOf(summary?.by_day, (d) => d.refused)),
      cacheHitRate: hitRate(
        Number(totals[0]?.cachedTokens ?? 0),
        Number(cacheable[0]?.promptTokens ?? 0),
      ),
    },
    byDay,
    byModel: [...byModel.values()].sort(bySpendThenRequests),
    byKey: await keyRows(organizationId, keys, summary, generations),
    refusals: (summary?.refusals ?? []).map((r) => ({
      limitSource: r.limit_source,
      reason: r.reason,
      statusCode: r.status_code,
      requests: r.requests,
    })),
    timings: summary
      ? {
          measured: summary.timings.measured,
          streamed: summary.timings.streamed,
          timeToFirstToken: summary.timings.time_to_first_token_ms,
          latency: summary.timings.latency_ms,
          generationTimeAvg: summary.timings.generation_time_ms_avg,
          throughputAvg: summary.timings.throughput_tokens_per_second_avg,
        }
      : null,
    requestLogAvailable: available,
  };
};

const modelKey = (provider: string, model: string) =>
  `${provider}\u0000${model}`;

const sumOf = <T>(rows: T[] | undefined, value: (row: T) => number) =>
  rows?.reduce((sum, row) => sum + value(row), 0);

const hitRate = (cached: number, prompt: number) =>
  prompt > 0 ? cached / prompt : null;

const bySpendThenRequests = (a: ActivityFigures, b: ActivityFigures) =>
  b.spend - a.spend || (b.requests ?? 0) - (a.requests ?? 0);

/**
 * Per API key: the ledger's spend and the log's requests, joined on the
 * gateway virtual key. A key deleted since keeps its numbers, under no name.
 */
const keyRows = async (
  organizationId: string,
  ledgerRows: (LedgerRow & { virtualKeyId: string | null })[],
  summary: GatewayGenerationSummary | undefined,
  generations: (n: number | undefined) => number | null,
): Promise<Activity["byKey"]> => {
  const logRows = new Map(
    (summary?.by_virtual_key ?? []).map((row) => [
      row.virtual_key_id,
      row.generations,
    ]),
  );
  const virtualKeyIds = [
    ...new Set([
      ...ledgerRows.flatMap((row) =>
        row.virtualKeyId ? [row.virtualKeyId] : [],
      ),
      ...logRows.keys(),
    ]),
  ].filter(Boolean);
  const named =
    virtualKeyIds.length > 0
      ? await db
          .select({
            id: apiKey.id,
            name: apiKey.name,
            prefix: apiKey.prefix,
            virtualKeyId: apiKey.virtualKeyId,
          })
          .from(apiKey)
          .where(
            and(
              eq(apiKey.organizationId, organizationId),
              inArray(apiKey.virtualKeyId, virtualKeyIds),
            ),
          )
      : [];
  const keyOf = new Map(named.map((key) => [key.virtualKeyId, key]));
  const rows = new Map<string, Activity["byKey"][number]>();
  const add = (virtualKeyId: string | null, figures: ActivityFigures) => {
    const key = virtualKeyId ? keyOf.get(virtualKeyId) : undefined;
    const id = key?.id ?? null;
    const known = rows.get(id ?? "");
    if (!known) {
      rows.set(id ?? "", {
        keyId: id,
        name: key?.name ?? null,
        prefix: key?.prefix ?? null,
        ...figures,
      });
      return;
    }
    known.spend += figures.spend;
    known.promptTokens += figures.promptTokens;
    known.completionTokens += figures.completionTokens;
    known.reasoningTokens += figures.reasoningTokens;
    known.cachedTokens += figures.cachedTokens;
    known.cacheWriteTokens += figures.cacheWriteTokens;
    if (known.requests !== null && figures.requests !== null) {
      known.requests += figures.requests;
    }
  };
  for (const row of ledgerRows) {
    add(
      row.virtualKeyId,
      toFigures(row, generations(logRows.get(row.virtualKeyId ?? ""))),
    );
    logRows.delete(row.virtualKeyId ?? "");
  }
  // keys that only made free or failed requests
  for (const [virtualKeyId, n] of logRows) {
    add(virtualKeyId || null, toFigures(undefined, n));
  }
  return [...rows.values()].sort(bySpendThenRequests);
};

/**
 * The gateway's summary of the organization's requests; an empty one when the
 * organization has never had a key, undefined when the log cannot be read.
 */
const organizationSummary = async (
  organizationId: string,
  from: Date,
  to: Date,
  gateway?: GatewayClient,
): Promise<GatewayGenerationSummary | undefined> => {
  const [mapping] = await db
    .select({ customerId: gatewayCustomer.customerId })
    .from(gatewayCustomer)
    .where(eq(gatewayCustomer.organizationId, organizationId));
  if (!mapping) {
    return EMPTY_SUMMARY;
  }
  try {
    return await (gateway ?? getGateway()).getGenerationSummary(
      mapping.customerId,
      from,
      to,
    );
  } catch (error) {
    if (
      error instanceof GatewayError ||
      error instanceof GatewayNotConfiguredError
    ) {
      logger.warn("gateway request log unavailable for activity", {
        organizationId,
        error,
      });
      return undefined;
    }
    throw error;
  }
};

const ZERO_LATENCY = { avg: 0, p50: 0, p90: 0 };

const EMPTY_SUMMARY: GatewayGenerationSummary = {
  by_day: [],
  by_model: [],
  by_virtual_key: [],
  refusals: [],
  timings: {
    measured: 0,
    streamed: 0,
    time_to_first_token_ms: ZERO_LATENCY,
    latency_ms: ZERO_LATENCY,
    generation_time_ms_avg: 0,
    throughput_tokens_per_second_avg: 0,
  },
};
