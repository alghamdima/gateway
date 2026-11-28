import { and, desc, eq, sql } from "@workspace/db";
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
import { CURRENCY, MICRO_USD, RATE_E6 } from "@workspace/shared/utils";

import { MICRO_SAR } from "../credits/ledger";
import { getDataEvidence } from "../data-policy/evidence";

import type { DataEvidence } from "../data-policy/evidence";
import type {
  GatewayAttempt,
  GatewayClient,
  GatewayLog,
} from "@workspace/gateway-client/server";

/**
 * One request's record (step 9.2), in OpenRouter's generation shape (research
 * brief 06). What was charged comes from the ledger, kept for good; timing and
 * the finish reason come from the gateway's log while it is kept (ADR 0014).
 * A request id can be chosen by the caller, so everything is looked up within
 * the asking organization: its ledger entries, and a gateway log only when it
 * belongs to the organization's own gateway customer.
 */

export interface Generation {
  id: string;
  /** The same id: one API call is one request and one generation for us. */
  request_id: string;
  created_at: string | null;
  model: string | null;
  provider_name: string | null;
  tokens_prompt: number;
  tokens_completion: number;
  /**
   * SAR charged (the account's currency, ADR 0017): the upstream cost at the
   * effective USD to SAR rate, with no markup (ADR 0010).
   */
  total_cost: number;
  /** The providers' cost in USD: what the charge was converted from. */
  upstream_cost_usd: number;
  currency: typeof CURRENCY;
  /**
   * The provider's own token counts (they are what we bill), summed over the
   * charged attempts; from the gateway's log for a request nothing was charged
   * for. Reasoning is part of the completion, cached and cache write part of
   * the prompt.
   */
  native_tokens_prompt: number;
  native_tokens_completion: number;
  native_tokens_reasoning: number;
  native_tokens_cached: number;
  native_tokens_cache_write: number;
  /**
   * Milliseconds, as OpenRouter's: time to first token for a stream, the whole
   * latency otherwise, on the attempt that answered. Null once the gateway's
   * log has expired.
   */
  latency: number | null;
  /** Milliseconds the provider took on the attempt that answered. */
  generation_time: number | null;
  /** Milliseconds, request received to response finished, on the attempt that answered. */
  total_latency: number | null;
  /** Completion tokens per second of generation time. */
  tokens_per_second: number | null;
  /** True when the client went away before the answer finished; null without the log. */
  cancelled: boolean | null;
  finish_reason: string | null;
  streamed: boolean | null;
  /**
   * Every attempt, the first and each fallback, failed ones at 0 cost
   * (ROUTE-04); from the gateway's log while it is kept, else the charged
   * attempts in the ledger.
   */
  provider_responses: {
    provider: string | null;
    model: string | null;
    status: "success" | "error";
    /** The HTTP status the attempt ended with (200 on success); null from the ledger alone. */
    status_code: number | null;
    /** Milliseconds; null from the ledger alone. */
    latency: number | null;
    /** 0 for the first attempt, 1 and up for fallbacks. */
    fallback_index: number;
    is_byok: false;
    /** SAR charged for the attempt. */
    cost: number;
    /** The provider's cost in USD. */
    upstream_cost_usd: number;
    /** The effective USD to SAR rate the attempt was converted at. */
    usd_to_sar_rate: number;
    created_at: string;
  }[];
  /** Where the serving endpoint processes data; null when unknown. */
  data_region: string | null;
  /** The endpoint that served the request and the policy in force then (ADR 0015). */
  data_policy: DataEvidence;
}

const metadataText = (key: string) =>
  sql<string | null>`${creditLedgerEntry.metadata}->>${key}`;
const metadataNumber = (key: string) =>
  sql<string | null>`(${creditLedgerEntry.metadata}->>${key})::bigint`;

const attemptColumns = {
  reference: creditLedgerEntry.reference,
  amount: creditLedgerEntry.amount,
  costMicroUsd: creditLedgerEntry.costMicroUsd,
  effectiveUsdToSarRateE6: creditLedgerEntry.effectiveUsdToSarRateE6,
  createdAt: creditLedgerEntry.createdAt,
  model: metadataText("model"),
  provider: metadataText("provider"),
  providerKeyId: metadataText("provider_key_id"),
  failed: sql<
    boolean | null
  >`(${creditLedgerEntry.metadata}->>'failed')::boolean`,
  promptTokens: metadataNumber("prompt_tokens"),
  completionTokens: metadataNumber("completion_tokens"),
  fallbackIndex: metadataNumber("fallback_index"),
  reasoningTokens: creditLedgerEntry.reasoningTokens,
  cachedTokens: creditLedgerEntry.cachedTokens,
  cacheWriteTokens: creditLedgerEntry.cacheWriteTokens,
};

type LedgerAttempt = Awaited<
  ReturnType<ReturnType<typeof selectAttempts>["orderBy"]>
>[number];

const selectAttempts = () => db.select(attemptColumns).from(creditLedgerEntry);

const usageOf = (organizationId: string) =>
  and(
    eq(creditLedgerEntry.organizationId, organizationId),
    eq(creditLedgerEntry.kind, "usage"),
  );

export const getGeneration = async (
  organizationId: string,
  id: string,
  gateway?: GatewayClient,
): Promise<Generation | undefined> => {
  const [attempts, logged] = await Promise.all([
    selectAttempts()
      .where(and(usageOf(organizationId), eq(creditLedgerEntry.reference, id)))
      .orderBy(creditLedgerEntry.createdAt),
    organizationLog(organizationId, id, gateway),
  ]);
  const log = logged?.root;
  if (attempts.length === 0 && !log) {
    return undefined;
  }

  const last = attempts.at(-1);
  // the attempt that answered, or failing that the last one that reached a provider key
  const reached = attempts.filter((a) => a.providerKeyId).reverse();
  const served = reached.find((a) => !a.failed) ?? reached[0];
  const startedAt =
    attempts[0]?.createdAt ?? (log?.timestamp ? new Date(log.timestamp) : null);
  const evidence = await getDataEvidence({
    organizationId,
    providerKeyId: served?.providerKeyId ?? null,
    provider: served?.provider ?? null,
    at: startedAt ?? new Date(),
  });
  const answer = logged?.answer;
  const answerAttempt = logged?.answerAttempt;
  const sum = (value: (a: LedgerAttempt) => number | string | null) =>
    attempts.reduce((total, a) => total + Number(value(a) ?? 0), 0);
  // the ledger counts what was charged; a request charged nothing has its counts in the log
  const usage = attempts.length > 0 ? undefined : answer?.token_usage;
  const tokens = {
    prompt: usage ? (usage.prompt_tokens ?? 0) : sum((a) => a.promptTokens),
    completion: usage
      ? (usage.completion_tokens ?? 0)
      : sum((a) => a.completionTokens),
    reasoning: usage
      ? (usage.completion_tokens_details?.reasoning_tokens ?? 0)
      : sum((a) => a.reasoningTokens),
    cached: usage
      ? (usage.prompt_tokens_details?.cached_read_tokens ?? 0)
      : sum((a) => a.cachedTokens),
    cacheWrite: usage
      ? (usage.prompt_tokens_details?.cached_write_tokens ?? 0)
      : sum((a) => a.cacheWriteTokens),
  };
  const generationTime = answerAttempt?.upstream_latency ?? null;
  const totalLatency = answerAttempt?.latency ?? answer?.latency ?? null;
  const streamed = answer?.stream ?? log?.stream ?? null;
  return {
    id,
    request_id: id,
    data_region: evidence.endpoint?.region ?? null,
    data_policy: evidence,
    created_at: attempts[0]?.createdAt.toISOString() ?? log?.timestamp ?? null,
    model: answer?.model ?? log?.model ?? last?.model ?? null,
    provider_name: answer?.provider ?? log?.provider ?? last?.provider ?? null,
    tokens_prompt: tokens.prompt,
    tokens_completion: tokens.completion,
    native_tokens_prompt: tokens.prompt,
    native_tokens_completion: tokens.completion,
    native_tokens_reasoning: tokens.reasoning,
    native_tokens_cached: tokens.cached,
    native_tokens_cache_write: tokens.cacheWrite,
    total_cost: attempts.reduce((total, a) => total - a.amount, 0) / MICRO_SAR,
    upstream_cost_usd:
      attempts.reduce((total, a) => total + (a.costMicroUsd ?? 0), 0) /
      MICRO_USD,
    currency: CURRENCY,
    // a stream's first chunk, else the whole wait (OpenRouter's latency); a stream not timed falls back too
    latency: streamed
      ? (answerAttempt?.time_to_first_token ?? totalLatency)
      : totalLatency,
    generation_time: generationTime,
    total_latency: totalLatency,
    tokens_per_second:
      generationTime && generationTime > 0
        ? tokens.completion / (generationTime / 1000)
        : null,
    cancelled: answer ? answer.status === "cancelled" : null,
    finish_reason: answer?.stop_reason ?? log?.stop_reason ?? null,
    streamed,
    provider_responses: providerResponses(attempts, logged?.attempts ?? []),
  };
};

/** What the ledger charged for one attempt, in SAR, with its USD cost and rate. */
const charge = (a: LedgerAttempt) => ({
  cost: -a.amount / MICRO_SAR,
  upstream_cost_usd: (a.costMicroUsd ?? 0) / MICRO_USD,
  usd_to_sar_rate: (a.effectiveUsdToSarRateE6 ?? 0) / RATE_E6,
});

/**
 * Every attempt with its cost: the gateway's list when it has one, each matched
 * to what the ledger charged for it by fallback index (a failed attempt has no
 * entry, so it shows at 0); the ledger's charged attempts alone otherwise.
 */
const providerResponses = (
  ledger: LedgerAttempt[],
  logged: GatewayAttempt[],
): Generation["provider_responses"] => {
  if (logged.length === 0) {
    return ledger.map((a) => ({
      provider: a.provider,
      model: a.model,
      status: a.failed ? "error" : "success",
      status_code: null,
      latency: null,
      fallback_index: Number(a.fallbackIndex ?? 0),
      is_byok: false,
      ...charge(a),
      created_at: a.createdAt.toISOString(),
    }));
  }
  return logged.map((attempt) => {
    const charged = ledger.filter(
      (a) => Number(a.fallbackIndex ?? 0) === attempt.fallback_index,
    );
    const rate = charged.at(-1)?.effectiveUsdToSarRateE6 ?? 0;
    return {
      provider: attempt.provider,
      model: attempt.model,
      status: attempt.status === "success" ? "success" : "error",
      status_code: attempt.status_code ?? null,
      latency: attempt.latency ?? null,
      fallback_index: attempt.fallback_index,
      is_byok: false,
      cost: charged.reduce((total, a) => total + charge(a).cost, 0),
      upstream_cost_usd: charged.reduce(
        (total, a) => total + charge(a).upstream_cost_usd,
        0,
      ),
      usd_to_sar_rate: rate / RATE_E6,
      created_at: attempt.timestamp,
    };
  });
};

interface OrganizationLog {
  /** The request's first attempt, as logged. */
  root: GatewayLog;
  /** Every attempt, first then fallbacks; empty when the gateway could not list them. */
  attempts: GatewayAttempt[];
  /** The attempt that answered, or the last one tried. */
  answerAttempt?: GatewayAttempt;
  /** That attempt's own log row (the root when it answered). */
  answer?: GatewayLog;
}

/** The gateway's log of the request, if it is this organization's; never fails the lookup. */
const organizationLog = async (
  organizationId: string,
  id: string,
  gateway?: GatewayClient,
): Promise<OrganizationLog | undefined> => {
  const [mapping] = await db
    .select({ customerId: gatewayCustomer.customerId })
    .from(gatewayCustomer)
    .where(eq(gatewayCustomer.organizationId, organizationId));
  if (!mapping) {
    return undefined;
  }
  try {
    const client = gateway ?? getGateway();
    const [root, attempts] = await Promise.all([
      client.getLog(id),
      client.getAttempts(id).catch((error: unknown) => {
        // the attempts are a detail: the record still answers without them
        logger.warn("gateway attempts unavailable for a generation lookup", {
          id,
          error,
        });
        return [];
      }),
    ]);
    if (root?.customer_id !== mapping.customerId) {
      return undefined;
    }
    // a fallback row is the same customer's: the gateway nests it under the root
    const answerAttempt =
      attempts.find((a) => a.status === "success") ?? attempts.at(-1);
    const answer =
      !answerAttempt || answerAttempt.id === root.id
        ? root
        : ((await client.getLog(answerAttempt.id)) ?? undefined);
    return { root, attempts, answerAttempt, answer };
  } catch (error) {
    if (
      error instanceof GatewayError ||
      error instanceof GatewayNotConfiguredError
    ) {
      // the charge is in the ledger; timing is a nice-to-have
      logger.warn("gateway log unavailable for a generation lookup", {
        id,
        error,
      });
      return undefined;
    }
    throw error;
  }
};

export interface RecentRequest {
  id: string | null;
  createdAt: string;
  model: string | null;
  provider: string | null;
  keyName: string | null;
  keyPrefix: string | null;
  promptTokens: number;
  completionTokens: number;
  /** micro-SAR charged. */
  cost: number;
  /** The provider's cost in micro-USD; null on entries older than SAR. */
  costMicroUsd: number | null;
  failed: boolean;
}

/** The attempt charged last names the model and provider that answered. */
const latest = (key: string) =>
  sql<
    string | null
  >`(array_agg(${creditLedgerEntry.metadata}->>${key} order by ${creditLedgerEntry.createdAt} desc))[1]`;

/**
 * The organization's latest charged requests, newest first, one row per
 * request however many of its attempts were charged; `before` is the last
 * row's time. Free requests and failed attempts cost nothing and are not in the
 * ledger: the full request list is the logs page's (ACT-07).
 */
export const listRecentRequests = async ({
  organizationId,
  limit,
  before,
}: {
  organizationId: string;
  limit: number;
  before?: Date;
}): Promise<RecentRequest[]> => {
  const createdAt = sql<Date>`max(${creditLedgerEntry.createdAt})`;
  const rows = await db
    .select({
      reference: creditLedgerEntry.reference,
      createdAt,
      amount: sql<string>`sum(${creditLedgerEntry.amount})`,
      costMicroUsd: sql<string | null>`sum(${creditLedgerEntry.costMicroUsd})`,
      model: latest("model"),
      provider: latest("provider"),
      failed: sql<boolean>`bool_and(coalesce((${creditLedgerEntry.metadata}->>'failed')::boolean, false))`,
      promptTokens: sql<string>`coalesce(sum((${creditLedgerEntry.metadata}->>'prompt_tokens')::bigint), 0)`,
      completionTokens: sql<string>`coalesce(sum((${creditLedgerEntry.metadata}->>'completion_tokens')::bigint), 0)`,
      keyName: apiKey.name,
      keyPrefix: apiKey.prefix,
    })
    .from(creditLedgerEntry)
    .leftJoin(apiKey, eq(apiKey.virtualKeyId, creditLedgerEntry.virtualKeyId))
    .where(usageOf(organizationId))
    .groupBy(creditLedgerEntry.reference, apiKey.name, apiKey.prefix)
    .having(before ? sql`${createdAt} < ${before.toISOString()}` : undefined)
    .orderBy(desc(createdAt))
    .limit(limit);
  return rows.map((row) => ({
    id: row.reference,
    createdAt: new Date(row.createdAt).toISOString(),
    model: row.model,
    provider: row.provider,
    keyName: row.keyName,
    keyPrefix: row.keyPrefix,
    promptTokens: Number(row.promptTokens),
    completionTokens: Number(row.completionTokens),
    cost: -Number(row.amount),
    costMicroUsd: row.costMicroUsd === null ? null : Number(row.costMicroUsd),
    failed: row.failed,
  }));
};
