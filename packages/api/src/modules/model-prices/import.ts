import { isNull } from "@workspace/db";
import { modelPrice } from "@workspace/db/schema";
import { db } from "@workspace/db/server";

import { offeredModels } from "./datasheet";
import { rateColumns, targetKey } from "./prices";
import { hasAnyRate, ratesFromGateway } from "./rates";

import type { GatewayClient } from "@workspace/gateway-client/server";

/**
 * The one-time seed (docs/platform/adr/0018-model-price-table.md): every model
 * the gateway offers and that has no price of ours gets a version holding the
 * datasheet's effective values, the price the gateway charges today, so
 * charges do not change when our prices take over. Idempotent: a model with
 * a price is skipped, and a second run inserts nothing.
 */

const INSERT_BATCH = 200;

export interface ImportSummary {
  imported: number;
  /** Models that already have a price of ours. */
  existing: number;
  /** Models the datasheet does not price: the gateway cannot charge them either. */
  unpriced: number;
  /** Cost fields left to the datasheet because they cannot be stored, per provider/model. */
  skippedFields: Record<string, string[]>;
}

export const importFromGateway = async ({
  gateway,
  staffId,
  now = new Date(),
}: {
  gateway: GatewayClient;
  staffId: string;
  now?: Date;
}): Promise<ImportSummary> => {
  const [offered, priced] = await Promise.all([
    offeredModels(gateway),
    db
      .selectDistinct({
        provider: modelPrice.provider,
        model: modelPrice.model,
      })
      .from(modelPrice)
      .where(isNull(modelPrice.providerKeyId)),
  ]);
  const existing = new Set(
    priced.map((row) => targetKey({ ...row, providerKeyId: null })),
  );
  const summary: ImportSummary = {
    imported: 0,
    existing: 0,
    unpriced: 0,
    skippedFields: {},
  };
  const rows: (typeof modelPrice.$inferInsert)[] = [];

  for (const { provider, model, entry } of offered) {
    if (existing.has(targetKey({ provider, model, providerKeyId: null }))) {
      summary.existing += 1;
      continue;
    }
    const { rates, skipped } = entry?.effective
      ? ratesFromGateway(entry.effective)
      : { rates: undefined, skipped: [] };
    if (skipped.length > 0) {
      summary.skippedFields[`${provider}/${model}`] = skipped;
    }
    if (!rates || !hasAnyRate(rates)) {
      summary.unpriced += 1;
      continue;
    }
    rows.push({
      provider,
      model,
      providerKeyId: null,
      effectiveFrom: now,
      ...rateColumns(rates),
      source: "import",
      note: "Imported from the gateway's datasheet",
      createdBy: staffId,
      createdAt: now,
    });
  }

  for (let index = 0; index < rows.length; index += INSERT_BATCH) {
    const inserted = await db
      .insert(modelPrice)
      .values(rows.slice(index, index + INSERT_BATCH))
      // a second import running at the same instant inserts nothing twice
      .onConflictDoNothing()
      .returning({ id: modelPrice.id });
    summary.imported += inserted.length;
  }
  return summary;
};
