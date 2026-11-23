import { desc, eq } from "@workspace/db";
import {
  pricingChange,
  pricingSettings,
  providerMarkup,
  user,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";

import type { SelectProviderMarkup } from "@workspace/db/schema";

/**
 * Markup per provider and the top-up fee, set by staff
 * (docs/platform/adr/0010-admin-controlled-pricing.md). Every rate is in basis
 * points: 100 bps = 1%. Each change is written with an entry in
 * pricing_change, in the same transaction.
 */

export interface PricingSettings {
  defaultMarkupBps: number;
  topupFeeBps: number;
  /** The least fee a top-up pays, in micro-USD. */
  topupFeeMinMicroUsd: number;
  updatedAt: Date;
  updatedBy: string | null;
}

export interface Pricing {
  settings: PricingSettings;
  providers: SelectProviderMarkup[];
}

const SETTINGS_ID = 1;

export const getPricingSettings = async (): Promise<PricingSettings> => {
  const [settings] = await db
    .select({
      defaultMarkupBps: pricingSettings.defaultMarkupBps,
      topupFeeBps: pricingSettings.topupFeeBps,
      topupFeeMinMicroUsd: pricingSettings.topupFeeMinMicroUsd,
      updatedAt: pricingSettings.updatedAt,
      updatedBy: pricingSettings.updatedBy,
    })
    .from(pricingSettings)
    .where(eq(pricingSettings.id, SETTINGS_ID));

  if (!settings) {
    // the migration creates the row and nothing deletes it
    throw new Error("pricing_settings has no row: run the database migrations");
  }
  return settings;
};

export const getPricing = async (): Promise<Pricing> => {
  const [settings, providers] = await Promise.all([
    getPricingSettings(),
    db.select().from(providerMarkup).orderBy(providerMarkup.provider),
  ]);
  return { settings, providers };
};

/** The latest changes, newest first, with the name of the staff user who made each one. */
export const getPricingChanges = async (limit = 50) =>
  db
    .select({
      id: pricingChange.id,
      target: pricingChange.target,
      before: pricingChange.before,
      after: pricingChange.after,
      changedAt: pricingChange.changedAt,
      changedBy: pricingChange.changedBy,
      changedByName: user.name,
      changedByEmail: user.email,
    })
    .from(pricingChange)
    .leftJoin(user, eq(user.id, pricingChange.changedBy))
    .orderBy(desc(pricingChange.id))
    .limit(limit);

export interface UpdatePricingSettingsInput {
  defaultMarkupBps: number;
  topupFeeBps: number;
  topupFeeMinMicroUsd: number;
  staffId: string;
}

export const updatePricingSettings = async ({
  defaultMarkupBps,
  topupFeeBps,
  topupFeeMinMicroUsd,
  staffId,
}: UpdatePricingSettingsInput): Promise<PricingSettings> =>
  db.transaction(async (tx) => {
    // lock the row so two staff saving at once are recorded in order
    const [before] = await tx
      .select({
        defaultMarkupBps: pricingSettings.defaultMarkupBps,
        topupFeeBps: pricingSettings.topupFeeBps,
        topupFeeMinMicroUsd: pricingSettings.topupFeeMinMicroUsd,
      })
      .from(pricingSettings)
      .where(eq(pricingSettings.id, SETTINGS_ID))
      .for("update");

    if (!before) {
      throw new Error(
        "pricing_settings has no row: run the database migrations",
      );
    }

    const [after] = await tx
      .update(pricingSettings)
      .set({
        defaultMarkupBps,
        topupFeeBps,
        topupFeeMinMicroUsd,
        updatedAt: new Date(),
        updatedBy: staffId,
      })
      .where(eq(pricingSettings.id, SETTINGS_ID))
      .returning({
        defaultMarkupBps: pricingSettings.defaultMarkupBps,
        topupFeeBps: pricingSettings.topupFeeBps,
        topupFeeMinMicroUsd: pricingSettings.topupFeeMinMicroUsd,
        updatedAt: pricingSettings.updatedAt,
        updatedBy: pricingSettings.updatedBy,
      });

    if (!after) {
      throw new Error("pricing_settings row disappeared during the update");
    }

    await tx.insert(pricingChange).values({
      target: "settings",
      before,
      after: { defaultMarkupBps, topupFeeBps, topupFeeMinMicroUsd },
      changedBy: staffId,
    });

    return after;
  });

export interface SetProviderMarkupInput {
  provider: string;
  markupBps: number;
  staffId: string;
}

export const setProviderMarkup = async ({
  provider,
  markupBps,
  staffId,
}: SetProviderMarkupInput): Promise<SelectProviderMarkup> =>
  db.transaction(async (tx) => {
    const [before] = await tx
      .select({ markupBps: providerMarkup.markupBps })
      .from(providerMarkup)
      .where(eq(providerMarkup.provider, provider))
      .for("update");

    const values = {
      provider,
      markupBps,
      updatedAt: new Date(),
      updatedBy: staffId,
    };
    const [row] = await tx
      .insert(providerMarkup)
      .values(values)
      .onConflictDoUpdate({ target: providerMarkup.provider, set: values })
      .returning();

    if (!row) {
      throw new Error(`provider_markup for ${provider} was not written`);
    }

    await tx.insert(pricingChange).values({
      target: `provider:${provider}`,
      before: before ?? null,
      after: { markupBps },
      changedBy: staffId,
    });

    return row;
  });

export interface RemoveProviderMarkupInput {
  provider: string;
  staffId: string;
}

/** The provider falls back to the default markup. False when it had no row. */
export const removeProviderMarkup = async ({
  provider,
  staffId,
}: RemoveProviderMarkupInput): Promise<boolean> =>
  db.transaction(async (tx) => {
    const [removed] = await tx
      .delete(providerMarkup)
      .where(eq(providerMarkup.provider, provider))
      .returning({ markupBps: providerMarkup.markupBps });

    if (!removed) {
      return false;
    }

    await tx.insert(pricingChange).values({
      target: `provider:${provider}`,
      before: removed,
      after: null,
      changedBy: staffId,
    });
    return true;
  });
