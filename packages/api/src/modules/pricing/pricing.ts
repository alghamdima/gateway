import { desc, eq } from "@workspace/db";
import { pricingChange, pricingSettings, user } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { effectiveUsdToSarRateE6 } from "@workspace/shared/utils";

/**
 * The top-up fee and limits, and the USD to SAR rate, set by staff
 * (docs/platform/adr/0010-admin-controlled-pricing.md, 0017-sar-ledger.md).
 * A request is charged its provider's cost at the effective rate, with no
 * markup: the platform's revenue is the top-up fee. Every percentage is in
 * basis points: 100 bps = 1%. Money is micro-SAR. Each change is written with
 * an entry in pricing_change, in the same transaction.
 */

export interface PricingSettings {
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

/**
 * The self-serve refund window and the credit expiry (ADMIN-23). Each
 * purchase stores what they gave it (credit_lot), so a change applies to
 * purchases made after it; a window of 0 also switches refunds off at once.
 */
export interface CreditTerms {
  /** 0: self-serve refunds are off. */
  refundWindowHours: number;
  /** Null: credit never expires. */
  creditExpiryDays: number | null;
}

export interface Pricing {
  settings: PricingSettings;
  creditTerms: CreditTerms;
}

const SETTINGS_ID = 1;

export const getPricingSettings = async (): Promise<PricingSettings> => {
  const [settings] = await db
    .select({
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

export const getCreditTerms = async (): Promise<CreditTerms> => {
  const [terms] = await db
    .select({
      refundWindowHours: pricingSettings.refundWindowHours,
      creditExpiryDays: pricingSettings.creditExpiryDays,
    })
    .from(pricingSettings)
    .where(eq(pricingSettings.id, SETTINGS_ID));
  if (!terms) {
    throw new Error("pricing_settings has no row: run the database migrations");
  }
  return terms;
};

export const getPricing = async (): Promise<Pricing> => {
  const [settings, creditTerms] = await Promise.all([
    getPricingSettings(),
    getCreditTerms(),
  ]);
  return { settings, creditTerms };
};

/** A change of the credit terms, with the values before and after it for the audit log. */
export interface CreditTermsChange {
  creditTerms: CreditTerms;
  before: CreditTerms;
  after: CreditTerms;
}

/**
 * Sets the refund window and the credit expiry (ADMIN-23), recorded in
 * pricing_change (target "credit_terms") in the same transaction.
 */
export const updateCreditTerms = async ({
  staffId,
  ...values
}: CreditTerms & { staffId: string }): Promise<CreditTermsChange> =>
  db.transaction(async (tx) => {
    const terms = {
      refundWindowHours: pricingSettings.refundWindowHours,
      creditExpiryDays: pricingSettings.creditExpiryDays,
    };
    // lock the row so two staff saving at once are recorded in order
    const [before] = await tx
      .select(terms)
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
      .set({ ...values, updatedAt: new Date(), updatedBy: staffId })
      .where(eq(pricingSettings.id, SETTINGS_ID))
      .returning(terms);
    if (!after) {
      throw new Error("pricing_settings row disappeared during the update");
    }
    await tx.insert(pricingChange).values({
      target: "credit_terms",
      before,
      after,
      changedBy: staffId,
    });
    return { creditTerms: after, before, after };
  });

/**
 * The latest changes, newest first, with the name of the staff user who made
 * each one. Rows written before the markup was removed keep their values
 * (target "provider:<name>", markup fields): the history is append-only.
 */
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
