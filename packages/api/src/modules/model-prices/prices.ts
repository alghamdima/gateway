import {
  and,
  asc,
  desc,
  eq,
  gt,
  ilike,
  isNotNull,
  isNull,
  lte,
  sql,
} from "@workspace/db";
import { modelPrice } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import { getPricingSettings } from "../pricing/pricing";
import { hasAnyRate, normalizeRates, ratesByField, usdToSar } from "./rates";

import type { Rates } from "./rates";
import type { CreateModelPriceInput } from "./schema";
import type { ModelPriceSource } from "@workspace/db/schema";

/**
 * Our model prices (ADMIN-09, docs/platform/adr/0018-model-price-table.md).
 * A version is never changed: the price of a target at time T is its version
 * with the greatest effective_from <= T. A target is a provider's model, or a
 * provider key's model (a negotiated price), which wins over the provider's.
 */

/** How far before now staff may set an effective time: a clock a little behind is not history. */
const PAST_TOLERANCE_MS = 60_000;
const MAX_LISTED = 2000;

export interface PriceTarget {
  provider: string;
  model: string;
  /** Null for the provider-wide price. */
  providerKeyId: string | null;
}

export interface ModelPriceVersion extends PriceTarget, Rates {
  id: number;
  effectiveFrom: Date;
  isRemoved: boolean;
  source: ModelPriceSource;
  suggestionId: number | null;
  note: string;
  createdBy: string;
  createdAt: Date;
}

type Row = typeof modelPrice.$inferSelect;

export const toVersion = (row: Row): ModelPriceVersion => ({
  id: row.id,
  provider: row.provider,
  model: row.model,
  providerKeyId: row.providerKeyId,
  effectiveFrom: row.effectiveFrom,
  isRemoved: row.isRemoved,
  ...normalizeRates({
    inputUsd: row.inputUsd,
    outputUsd: row.outputUsd,
    cacheReadUsd: row.cacheReadUsd,
    cacheWriteUsd: row.cacheWriteUsd,
    cacheWrite1hUsd: row.cacheWrite1hUsd,
    otherRates: row.otherRates,
  }),
  source: row.source,
  suggestionId: row.suggestionId,
  note: row.note,
  createdBy: row.createdBy,
  createdAt: row.createdAt,
});

export const targetKey = ({ provider, model, providerKeyId }: PriceTarget) =>
  JSON.stringify([provider, model.toLowerCase(), providerKeyId]);

/** The rates of a version as stored: the typed columns and other_rates. */
export const rateColumns = (rates: Rates) => {
  const normalized = normalizeRates(rates);
  return {
    inputUsd: normalized.inputUsd,
    outputUsd: normalized.outputUsd,
    cacheReadUsd: normalized.cacheReadUsd,
    cacheWriteUsd: normalized.cacheWriteUsd,
    cacheWrite1hUsd: normalized.cacheWrite1hUsd,
    otherRates: normalized.otherRates,
  };
};

const targetIs = ({ provider, model, providerKeyId }: PriceTarget) =>
  and(
    eq(modelPrice.provider, provider),
    eq(modelPrice.model, model),
    providerKeyId === null
      ? isNull(modelPrice.providerKeyId)
      : eq(modelPrice.providerKeyId, providerKeyId),
  );

/**
 * The version of one target in force at `at`, a removal included (a key's
 * price that ended); undefined when the target had none yet.
 */
export const getVersionAt = async (
  target: PriceTarget,
  at: Date,
  executor: Pick<typeof db, "select"> = db,
): Promise<ModelPriceVersion | undefined> => {
  const [row] = await executor
    .select()
    .from(modelPrice)
    .where(and(targetIs(target), lte(modelPrice.effectiveFrom, at)))
    .orderBy(desc(modelPrice.effectiveFrom))
    .limit(1);
  return row ? toVersion(row) : undefined;
};

/**
 * The price a request served at `at` through `providerKeyId` is charged: the
 * key's own price in force, else the provider's. Undefined when neither has one.
 */
export const resolvePriceAt = async ({
  provider,
  model,
  providerKeyId,
  at,
}: {
  provider: string;
  model: string;
  providerKeyId?: string | null;
  at: Date;
}): Promise<ModelPriceVersion | undefined> => {
  if (providerKeyId) {
    const own = await getVersionAt({ provider, model, providerKeyId }, at);
    if (own && !own.isRemoved) {
      return own;
    }
  }
  const shared = await getVersionAt(
    { provider, model, providerKeyId: null },
    at,
  );
  return shared?.isRemoved ? undefined : shared;
};

export interface CurrentPricesFilter {
  provider?: string;
  q?: string;
  scope?: "provider" | "key";
}

/**
 * The version in force at `at` of every target, removals left out: one
 * DISTINCT ON over the target's unique index.
 */
export const getVersionsInForce = async (
  at: Date,
  { provider, q, scope }: CurrentPricesFilter = {},
  {
    includeRemoved = false,
    limit,
  }: { includeRemoved?: boolean; limit?: number } = {},
): Promise<ModelPriceVersion[]> => {
  const query = db
    .selectDistinctOn([
      modelPrice.provider,
      modelPrice.model,
      modelPrice.providerKeyId,
    ])
    .from(modelPrice)
    .where(
      and(
        lte(modelPrice.effectiveFrom, at),
        provider ? eq(modelPrice.provider, provider) : undefined,
        q ? ilike(modelPrice.model, `%${escapeLike(q)}%`) : undefined,
        scope === "provider" ? isNull(modelPrice.providerKeyId) : undefined,
        scope === "key" ? isNotNull(modelPrice.providerKeyId) : undefined,
      ),
    )
    .orderBy(
      asc(modelPrice.provider),
      asc(modelPrice.model),
      asc(modelPrice.providerKeyId),
      desc(modelPrice.effectiveFrom),
    )
    .$dynamic();
  const rows = await (limit === undefined ? query : query.limit(limit));
  return rows
    .map(toVersion)
    .filter((version) => includeRemoved || !version.isRemoved);
};

const escapeLike = (value: string) => value.replace(/[\\%_]/g, "\\$&");

/** A rate in USD and in SAR at the effective rate, both per unit. */
export interface PricedRate {
  field: string;
  usd: string;
  sar: string;
}

export interface CurrentModelPrice extends ModelPriceVersion {
  rates: PricedRate[];
  /** The next version already saved for this target, if any. */
  scheduledFrom: Date | null;
}

/** Every rate of a version, in USD and in SAR at `rateE6`. */
export const pricedRates = (rates: Rates, rateE6: number): PricedRate[] =>
  Object.entries(ratesByField(rates)).map(([field, usd]) => ({
    field,
    usd,
    sar: usdToSar(usd, rateE6),
  }));

/** The admin list: what each target costs now, in USD and in SAR at today's effective rate. */
export const listCurrentPrices = async (
  filter: CurrentPricesFilter,
  now = new Date(),
): Promise<{
  effectiveUsdToSarRateE6: number;
  prices: CurrentModelPrice[];
}> => {
  const [versions, settings, scheduled] = await Promise.all([
    getVersionsInForce(now, filter, { limit: MAX_LISTED }),
    getPricingSettings(),
    db
      .select({
        provider: modelPrice.provider,
        model: modelPrice.model,
        providerKeyId: modelPrice.providerKeyId,
        next: sql<Date>`min(${modelPrice.effectiveFrom})`.mapWith(
          (value: string | Date) => new Date(value),
        ),
      })
      .from(modelPrice)
      .where(gt(modelPrice.effectiveFrom, now))
      .groupBy(modelPrice.provider, modelPrice.model, modelPrice.providerKeyId),
  ]);
  const nextByTarget = new Map(
    scheduled.map((row) => [targetKey(row), row.next]),
  );
  const rateE6 = settings.effectiveUsdToSarRateE6;
  return {
    effectiveUsdToSarRateE6: rateE6,
    prices: versions.map((version) => ({
      ...version,
      rates: pricedRates(version, rateE6),
      scheduledFrom: nextByTarget.get(targetKey(version)) ?? null,
    })),
  };
};

/** Every version of a model at a provider, its keys' included, newest first. */
export const getPriceHistory = async (
  { provider, model }: { provider: string; model: string },
  now = new Date(),
): Promise<(ModelPriceVersion & { isScheduled: boolean })[]> => {
  const rows = await db
    .select()
    .from(modelPrice)
    .where(and(eq(modelPrice.provider, provider), eq(modelPrice.model, model)))
    .orderBy(desc(modelPrice.effectiveFrom), desc(modelPrice.id))
    .limit(MAX_LISTED);
  return rows.map((row) => ({
    ...toVersion(row),
    isScheduled: row.effectiveFrom.getTime() > now.getTime(),
  }));
};

/** The values a change records: the audit log keeps the same. */
export const versionValues = (version: ModelPriceVersion | undefined) =>
  version
    ? {
        id: version.id,
        provider: version.provider,
        model: version.model,
        providerKeyId: version.providerKeyId,
        effectiveFrom: version.effectiveFrom.toISOString(),
        isRemoved: version.isRemoved,
        source: version.source,
        rates: ratesByField(version),
      }
    : null;

export interface CreatedVersion {
  version: ModelPriceVersion;
  /** What was in force when it was saved, for the audit log. */
  before: ReturnType<typeof versionValues>;
  after: ReturnType<typeof versionValues>;
}

export interface CreateVersionInput extends CreateModelPriceInput {
  staffId: string;
  source?: ModelPriceSource;
  suggestionId?: number | null;
  now?: Date;
}

/**
 * Saves a new version. It never touches an existing one: a later
 * effective_from supersedes. Refused: an effective time in the past (history
 * already charged), a second version at the same instant (409), and a key's
 * price that leaves out a rate the provider's price sets (the gateway applies
 * one override per request and does not merge them, so the key's price must
 * be whole).
 */
export const createPriceVersion = async ({
  staffId,
  source = "manual",
  suggestionId = null,
  now = new Date(),
  ...input
}: CreateVersionInput): Promise<CreatedVersion> => {
  const effectiveFrom = input.effectiveFrom ?? now;
  if (effectiveFrom.getTime() < now.getTime() - PAST_TOLERANCE_MS) {
    throw new HttpException(HttpStatusCode.BAD_REQUEST, {
      code: "error.badRequest",
      message: "A price cannot start in the past: add a new version from now.",
    });
  }
  const target: PriceTarget = {
    provider: input.provider,
    model: input.model,
    providerKeyId: input.providerKeyId,
  };
  const rates = input.rates ? normalizeRates(input.rates) : undefined;
  if (!input.isRemoved && (!rates || !hasAnyRate(rates))) {
    throw new HttpException(HttpStatusCode.BAD_REQUEST, {
      code: "error.badRequest",
      message: "A price needs at least one rate.",
    });
  }

  const before = await getVersionAt(target, effectiveFrom);
  if (target.providerKeyId !== null && rates) {
    const shared = await getVersionAt(
      { ...target, providerKeyId: null },
      effectiveFrom,
    );
    const missing = shared
      ? Object.keys(ratesByField(shared)).filter(
          (field) => !(field in ratesByField(rates)),
        )
      : [];
    if (missing.length > 0) {
      throw new HttpException(HttpStatusCode.BAD_REQUEST, {
        code: "error.badRequest",
        message: `A key's price must set every rate the provider's price sets; missing: ${missing.join(", ")}`,
      });
    }
  }
  if (input.isRemoved && (!before || before.isRemoved)) {
    throw new HttpException(HttpStatusCode.BAD_REQUEST, {
      code: "error.badRequest",
      message: "This key has no price of its own to remove.",
    });
  }

  try {
    const [row] = await db
      .insert(modelPrice)
      .values({
        ...target,
        effectiveFrom,
        isRemoved: input.isRemoved,
        ...(rates ? rateColumns(rates) : {}),
        source,
        suggestionId,
        note: input.note,
        createdBy: staffId,
        createdAt: now,
      })
      .returning();
    if (!row) {
      throw new Error("model_price insert returned no row");
    }
    const version = toVersion(row);
    return {
      version,
      before: versionValues(before),
      after: versionValues(version),
    };
  } catch (error) {
    if (isUniqueViolation(error)) {
      throw new HttpException(HttpStatusCode.CONFLICT, {
        code: "error.conflict",
        message: "This model already has a version starting at that time.",
      });
    }
    throw error;
  }
};

export const isUniqueViolation = (error: unknown) =>
  hasCode(error, "23505") ||
  (error instanceof Error && hasCode(error.cause, "23505"));

const hasCode = (error: unknown, code: string) =>
  typeof error === "object" &&
  error !== null &&
  "code" in error &&
  error.code === code;
