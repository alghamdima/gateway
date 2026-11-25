import { and, desc, eq, lt, sql } from "@workspace/db";
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

import { MICRO_USD } from "../credits/ledger";
import { getDataEvidence } from "../data-policy/evidence";

import type { DataEvidence } from "../data-policy/evidence";
import type {
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
  created_at: string | null;
  model: string | null;
  provider_name: string | null;
  tokens_prompt: number;
  tokens_completion: number;
  /** USD charged, markup included. */
  total_cost: number;
  /** Milliseconds; null once the gateway's log has expired. */
  latency: number | null;
  finish_reason: string | null;
  streamed: boolean | null;
  provider_responses: {
    provider: string | null;
    model: string | null;
    status: "success" | "error";
    cost: number;
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
  createdAt: creditLedgerEntry.createdAt,
  model: metadataText("model"),
  provider: metadataText("provider"),
  providerKeyId: metadataText("provider_key_id"),
  failed: sql<
    boolean | null
  >`(${creditLedgerEntry.metadata}->>'failed')::boolean`,
  promptTokens: metadataNumber("prompt_tokens"),
  completionTokens: metadataNumber("completion_tokens"),
};

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
  const [attempts, log] = await Promise.all([
    db
      .select(attemptColumns)
      .from(creditLedgerEntry)
      .where(and(usageOf(organizationId), eq(creditLedgerEntry.reference, id)))
      .orderBy(creditLedgerEntry.createdAt),
    organizationLog(organizationId, id, gateway),
  ]);
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
  return {
    id,
    data_region: evidence.endpoint?.region ?? null,
    data_policy: evidence,
    created_at: attempts[0]?.createdAt.toISOString() ?? log?.timestamp ?? null,
    model: log?.model ?? last?.model ?? null,
    provider_name: log?.provider ?? last?.provider ?? null,
    tokens_prompt: attempts.reduce(
      (sum, a) => sum + Number(a.promptTokens ?? 0),
      0,
    ),
    tokens_completion: attempts.reduce(
      (sum, a) => sum + Number(a.completionTokens ?? 0),
      0,
    ),
    total_cost: attempts.reduce((sum, a) => sum - a.amount, 0) / MICRO_USD,
    latency: log?.latency ?? null,
    finish_reason: log?.stop_reason ?? null,
    streamed: log?.stream ?? null,
    provider_responses: attempts.map((a) => ({
      provider: a.provider,
      model: a.model,
      status: a.failed ? "error" : "success",
      cost: -a.amount / MICRO_USD,
      created_at: a.createdAt.toISOString(),
    })),
  };
};

/** The gateway's log of the request, if it is this organization's; never fails the lookup. */
const organizationLog = async (
  organizationId: string,
  id: string,
  gateway?: GatewayClient,
): Promise<GatewayLog | undefined> => {
  const [mapping] = await db
    .select({ customerId: gatewayCustomer.customerId })
    .from(gatewayCustomer)
    .where(eq(gatewayCustomer.organizationId, organizationId));
  if (!mapping) {
    return undefined;
  }
  try {
    const log = await (gateway ?? getGateway()).getLog(id);
    return log?.customer_id === mapping.customerId ? log : undefined;
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
  /** micro-USD. */
  cost: number;
  failed: boolean;
}

/** The organization's latest charged attempts, newest first; `before` is the last one's time. */
export const listRecentRequests = async ({
  organizationId,
  limit,
  before,
}: {
  organizationId: string;
  limit: number;
  before?: Date;
}): Promise<RecentRequest[]> => {
  const rows = await db
    .select({
      ...attemptColumns,
      keyName: apiKey.name,
      keyPrefix: apiKey.prefix,
    })
    .from(creditLedgerEntry)
    .leftJoin(apiKey, eq(apiKey.virtualKeyId, creditLedgerEntry.virtualKeyId))
    .where(
      and(
        usageOf(organizationId),
        before ? lt(creditLedgerEntry.createdAt, before) : undefined,
      ),
    )
    .orderBy(desc(creditLedgerEntry.createdAt))
    .limit(limit);
  return rows.map((row) => ({
    id: row.reference,
    createdAt: row.createdAt.toISOString(),
    model: row.model,
    provider: row.provider,
    keyName: row.keyName,
    keyPrefix: row.keyPrefix,
    promptTokens: Number(row.promptTokens ?? 0),
    completionTokens: Number(row.completionTokens ?? 0),
    cost: -row.amount,
    failed: row.failed === true,
  }));
};
