import { desc, eq } from "@workspace/db";
import {
  pricingChange,
  pricingSettings,
  providerMarkup,
  user,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { effectiveUsdToSarRateE6 } from "@workspace/shared/utils";

import type { SelectProviderMarkup } from "@workspace/db/schema";

/**
 * Markup per provider, the top-up fee and limits, and the USD to SAR rate,
 * set by staff (docs/platform/adr/0010-admin-controlled-pricing.md,
 * 0017-sar-ledger.md). Every percentage is in basis points: 100 bps = 1%.
 * Money is micro-SAR. Each change is written with an entry in pricing_change,
 * in the same transaction.
 */

export interface PricingSettings {
  defaultMarkupBps: number;
  topupFeeBps: number;
  /** The least fee a top-up pays, in micro-SAR. */
  topupFeeMinMicroSar: number;
  /** The least credit one top-up may buy, in micro-SAR. */
  topupMinMicroSar: number;
  /** The most credit one top-up may buy, in micro-SAR. */
  topupMaxMicroSar: number;
  /** The quick-pick amounts of the top-up form, in micro-SAR. */
  topupPresetsMicroSar: number[];
  /** The official USD to SAR rate × 1,000,000 (ADMIN-06). */
  usdToSarRateE6: number;
  /** The bank's conversion commission in bps, added to the official rate. */
  fxSpreadBps: number;
  /** What new requests convert at: official plus spread, rounded up. Computed. */
  effectiveUsdToSarRateE6: number;
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
      topupFeeMinMicroSar: pricingSettings.topupFeeMinMicroSar,
      topupMinMicroSar: pricingSettings.topupMinMicroSar,
      topupMaxMicroSar: pricingSettings.topupMaxMicroSar,
      topupPresetsMicroSar: pricingSettings.topupPresetsMicroSar,
      usdToSarRateE6: pricingSettings.usdToSarRateE6,
      fxSpreadBps: pricingSettings.fxSpreadBps,
      updatedAt: pricingSettings.updatedAt,
      updatedBy: pricingSettings.updatedBy,
    })
    .from(pricingSettings)
    .where(eq(pricingSettings.id, SETTINGS_ID));

  if (!settings) {
    // the migration creates the row and nothing deletes it
    throw new Error("pricing_settings has no row: run the database migrations");
  }
  return withEffectiveRate(settings);
};

const withEffectiveRate = <
  T extends { usdToSarRateE6: number; fxSpreadBps: number },
>(
  settings: T,
) => ({
  ...settings,
  effectiveUsdToSarRateE6: effectiveUsdToSarRateE6(
    settings.usdToSarRateE6,
    settings.fxSpreadBps,
  ),
});

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
  topupFeeMinMicroSar: number;
  topupMinMicroSar: number;
  topupMaxMicroSar: number;
  topupPresetsMicroSar: number[];
  usdToSarRateE6: number;
  fxSpreadBps: number;
  staffId: string;
}

type PricingValues = Omit<PricingSettings, "updatedAt" | "updatedBy">;

/** The settings after a change, and the values before and after it, for the audit log. */
export interface PricingSettingsChange {
  settings: PricingSettings;
  before: PricingValues;
  after: PricingValues;
}

export const updatePricingSettings = async ({
  staffId,
  ...input
}: UpdatePricingSettingsInput): Promise<PricingSettingsChange> =>
  db.transaction(async (tx) => {
    // the form shows each quick pick once, smallest first, whatever staff typed
    const values = {
      ...input,
      topupPresetsMicroSar: [...new Set(input.topupPresetsMicroSar)].sort(
        (a, b) => a - b,
      ),
    };
    // lock the row so two staff saving at once are recorded in order
    const [before] = await tx
      .select({
        defaultMarkupBps: pricingSettings.defaultMarkupBps,
        topupFeeBps: pricingSettings.topupFeeBps,
        topupFeeMinMicroSar: pricingSettings.topupFeeMinMicroSar,
        topupMinMicroSar: pricingSettings.topupMinMicroSar,
        topupMaxMicroSar: pricingSettings.topupMaxMicroSar,
        topupPresetsMicroSar: pricingSettings.topupPresetsMicroSar,
        usdToSarRateE6: pricingSettings.usdToSarRateE6,
        fxSpreadBps: pricingSettings.fxSpreadBps,
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
        ...values,
        updatedAt: new Date(),
        updatedBy: staffId,
      })
      .where(eq(pricingSettings.id, SETTINGS_ID))
      .returning({
        defaultMarkupBps: pricingSettings.defaultMarkupBps,
        topupFeeBps: pricingSettings.topupFeeBps,
        topupFeeMinMicroSar: pricingSettings.topupFeeMinMicroSar,
        topupMinMicroSar: pricingSettings.topupMinMicroSar,
        topupMaxMicroSar: pricingSettings.topupMaxMicroSar,
        topupPresetsMicroSar: pricingSettings.topupPresetsMicroSar,
        usdToSarRateE6: pricingSettings.usdToSarRateE6,
        fxSpreadBps: pricingSettings.fxSpreadBps,
        updatedAt: pricingSettings.updatedAt,
        updatedBy: pricingSettings.updatedBy,
      });

    if (!after) {
      throw new Error("pricing_settings row disappeared during the update");
    }

    // the effective rate is recorded beside the two values it comes from, so
    // the history shows what each change meant for new requests
    const change = {
      before: withEffectiveRate(before),
      after: withEffectiveRate(values),
    };
    await tx.insert(pricingChange).values({
      target: "settings",
      ...change,
      changedBy: staffId,
    });

    return { settings: withEffectiveRate(after), ...change };
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
