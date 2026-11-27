import { and, desc, eq } from "@workspace/db";
import { modelPrice, modelPriceSuggestion } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import { offeredModels } from "./datasheet";
import {
  getVersionAt,
  getVersionsInForce,
  rateColumns,
  targetKey,
  toVersion,
  versionValues,
} from "./prices";
import {
  hasAnyRate,
  normalizeRates,
  ratesByField,
  ratesEqual,
  ratesFromGateway,
} from "./rates";

import type { ModelPriceVersion } from "./prices";
import type { Rates } from "./rates";
import type { ModelPriceSuggestionStatus } from "@workspace/db/schema";
import type { GatewayClient } from "@workspace/gateway-client/server";

/**
 * Suggestions (docs/platform/adr/0018-model-price-table.md): the datasheet's
 * price for a model, when it differs from ours. A sync writes suggestions and
 * nothing else: a price staff set always wins. Approving one makes it a new
 * version; rejecting one leaves the price as it is.
 */

type Row = typeof modelPriceSuggestion.$inferSelect;

export interface ModelPriceSuggestion extends Rates {
  id: number;
  provider: string;
  model: string;
  source: string;
  basePriceId: number | null;
  status: ModelPriceSuggestionStatus;
  createdAt: Date;
  decidedBy: string | null;
  decidedAt: Date | null;
  reason: string;
}

const toSuggestion = (row: Row): ModelPriceSuggestion => ({
  id: row.id,
  provider: row.provider,
  model: row.model,
  source: row.source,
  basePriceId: row.basePriceId,
  ...normalizeRates({
    inputUsd: row.inputUsd,
    outputUsd: row.outputUsd,
    cacheReadUsd: row.cacheReadUsd,
    cacheWriteUsd: row.cacheWriteUsd,
    cacheWrite1hUsd: row.cacheWrite1hUsd,
    otherRates: row.otherRates,
  }),
  status: row.status,
  createdAt: row.createdAt,
  decidedBy: row.decidedBy,
  decidedAt: row.decidedAt,
  reason: row.reason,
});

export interface SuggestionWithBase extends ModelPriceSuggestion {
  /** The price it would replace, when there is one. */
  base: ModelPriceVersion | null;
}

/** The inbox: suggestions of one status, newest first, each beside the price it would replace. */
export const listSuggestions = async ({
  status,
}: {
  status: ModelPriceSuggestionStatus;
}): Promise<SuggestionWithBase[]> => {
  const rows = await db
    .select({ suggestion: modelPriceSuggestion, base: modelPrice })
    .from(modelPriceSuggestion)
    .leftJoin(modelPrice, eq(modelPrice.id, modelPriceSuggestion.basePriceId))
    .where(eq(modelPriceSuggestion.status, status))
    .orderBy(
      desc(modelPriceSuggestion.createdAt),
      desc(modelPriceSuggestion.id),
    )
    .limit(1000);
  return rows.map(({ suggestion, base }) => ({
    ...toSuggestion(suggestion),
    base: base ? toVersion(base) : null,
  }));
};

export interface SuggestionSync {
  checked: number;
  created: number;
  superseded: number;
  unchanged: number;
  /** Models the datasheet does not price. */
  unpriced: number;
}

/**
 * Compares the datasheet's upstream price of every model the gateway offers
 * with our provider-wide price in force, and proposes the difference. Never
 * writes a price. A suggestion already pending with the same rates is left
 * alone, the same rates rejected against the same price are not proposed
 * again, and a pending suggestion with other rates is superseded.
 */
export const syncSuggestions = async (
  gateway: GatewayClient,
  now = new Date(),
): Promise<SuggestionSync> => {
  const [offered, versions, pending] = await Promise.all([
    offeredModels(gateway),
    getVersionsInForce(now, { scope: "provider" }),
    db
      .select()
      .from(modelPriceSuggestion)
      .where(eq(modelPriceSuggestion.status, "pending")),
  ]);
  const priceByTarget = new Map(
    versions.map((version) => [targetKey(version), version]),
  );
  const pendingByTarget = new Map(
    pending.map((row) => [
      targetKey({ ...row, providerKeyId: null }),
      toSuggestion(row),
    ]),
  );
  const sync: SuggestionSync = {
    checked: 0,
    created: 0,
    superseded: 0,
    unchanged: 0,
    unpriced: 0,
  };

  for (const { provider, model, entry } of offered) {
    sync.checked += 1;
    const proposed = entry?.upstream
      ? ratesFromGateway(entry.upstream).rates
      : undefined;
    if (!proposed || !hasAnyRate(proposed)) {
      sync.unpriced += 1;
      continue;
    }
    const key = targetKey({ provider, model, providerKeyId: null });
    const current = priceByTarget.get(key);
    const open = pendingByTarget.get(key);
    if (current && ratesEqual(current, proposed)) {
      // the datasheet came back to our price: nothing to propose any more
      if (open) {
        await supersede(open.id, now);
        sync.superseded += 1;
      } else {
        sync.unchanged += 1;
      }
      continue;
    }
    if (
      open &&
      ratesEqual(open, proposed) &&
      open.basePriceId === (current?.id ?? null)
    ) {
      sync.unchanged += 1;
      continue;
    }
    if (await wasRejected(provider, model, current?.id ?? null, proposed)) {
      sync.unchanged += 1;
      continue;
    }
    await db.transaction(async (tx) => {
      if (open) {
        await tx
          .update(modelPriceSuggestion)
          .set({ status: "superseded", decidedAt: now })
          .where(
            and(
              eq(modelPriceSuggestion.id, open.id),
              eq(modelPriceSuggestion.status, "pending"),
            ),
          );
      }
      await tx.insert(modelPriceSuggestion).values({
        provider,
        model,
        source: "datasheet",
        basePriceId: current?.id ?? null,
        ...rateColumns(proposed),
        createdAt: now,
      });
    });
    if (open) {
      sync.superseded += 1;
    }
    sync.created += 1;
  }
  return sync;
};

const supersede = async (id: number, now: Date) => {
  await db
    .update(modelPriceSuggestion)
    .set({ status: "superseded", decidedAt: now })
    .where(
      and(
        eq(modelPriceSuggestion.id, id),
        eq(modelPriceSuggestion.status, "pending"),
      ),
    );
};

const wasRejected = async (
  provider: string,
  model: string,
  basePriceId: number | null,
  proposed: Rates,
) => {
  const rejected = await db
    .select()
    .from(modelPriceSuggestion)
    .where(
      and(
        eq(modelPriceSuggestion.provider, provider),
        eq(modelPriceSuggestion.model, model),
        eq(modelPriceSuggestion.status, "rejected"),
      ),
    )
    .orderBy(desc(modelPriceSuggestion.id))
    .limit(20);
  return rejected
    .map(toSuggestion)
    .some(
      (row) => row.basePriceId === basePriceId && ratesEqual(row, proposed),
    );
};

const notPending = () =>
  new HttpException(HttpStatusCode.CONFLICT, {
    code: "error.conflict",
    message: "This suggestion was already decided.",
  });

const lockPending = async (
  tx: Parameters<Parameters<typeof db.transaction>[0]>[0],
  id: number,
) => {
  const [row] = await tx
    .select()
    .from(modelPriceSuggestion)
    .where(eq(modelPriceSuggestion.id, id))
    .for("update");
  if (!row) {
    throw new HttpException(HttpStatusCode.NOT_FOUND, {
      code: "error.notFound",
    });
  }
  if (row.status !== "pending") {
    throw notPending();
  }
  return toSuggestion(row);
};

const suggestionValues = (suggestion: ModelPriceSuggestion) => ({
  id: suggestion.id,
  provider: suggestion.provider,
  model: suggestion.model,
  status: suggestion.status,
  basePriceId: suggestion.basePriceId,
  rates: ratesByField(suggestion),
});

/**
 * Makes a pending suggestion a new provider-wide version. Refused (409) when
 * it is no longer pending, or when the price in force is no longer the one it
 * was made against: a price staff set after it wins, and the suggestion has
 * to be looked at again.
 */
export const approveSuggestion = async ({
  id,
  staffId,
  effectiveFrom,
  reason = "",
  now = new Date(),
}: {
  id: number;
  staffId: string;
  effectiveFrom?: Date;
  reason?: string;
  now?: Date;
}) => {
  const from = effectiveFrom ?? now;
  if (from.getTime() < now.getTime() - 60_000) {
    throw new HttpException(HttpStatusCode.BAD_REQUEST, {
      code: "error.badRequest",
      message: "A price cannot start in the past.",
    });
  }
  return db.transaction(async (tx) => {
    const suggestion = await lockPending(tx, id);
    const target = {
      provider: suggestion.provider,
      model: suggestion.model,
      providerKeyId: null,
    };
    const current = await getVersionAt(target, now, tx);
    if ((current?.id ?? null) !== suggestion.basePriceId) {
      throw new HttpException(HttpStatusCode.CONFLICT, {
        code: "error.conflict",
        message:
          "The price changed after this suggestion was made; it can only be rejected.",
      });
    }
    const [row] = await tx
      .insert(modelPrice)
      .values({
        ...target,
        effectiveFrom: from,
        ...rateColumns(suggestion),
        source: "suggestion",
        suggestionId: suggestion.id,
        note: reason,
        createdBy: staffId,
        createdAt: now,
      })
      .returning();
    if (!row) {
      throw new Error("model_price insert returned no row");
    }
    await tx
      .update(modelPriceSuggestion)
      .set({ status: "approved", decidedBy: staffId, decidedAt: now, reason })
      .where(eq(modelPriceSuggestion.id, id));
    const version = toVersion(row);
    return {
      version,
      before: {
        suggestion: suggestionValues(suggestion),
        price: versionValues(current),
      },
      after: {
        suggestion: { ...suggestionValues(suggestion), status: "approved" },
        price: versionValues(version),
      },
    };
  });
};

/** Closes a pending suggestion with a reason; the price stays as it is. */
export const rejectSuggestion = async ({
  id,
  staffId,
  reason,
  now = new Date(),
}: {
  id: number;
  staffId: string;
  reason: string;
  now?: Date;
}) =>
  db.transaction(async (tx) => {
    const suggestion = await lockPending(tx, id);
    await tx
      .update(modelPriceSuggestion)
      .set({ status: "rejected", decidedBy: staffId, decidedAt: now, reason })
      .where(eq(modelPriceSuggestion.id, id));
    return {
      before: suggestionValues(suggestion),
      after: { ...suggestionValues(suggestion), status: "rejected", reason },
    };
  });
