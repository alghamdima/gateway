import { getBillingProvider } from "@workspace/billing-web/server";
import { desc, eq } from "@workspace/db";
import {
  paymentChange,
  paymentProvider,
  paymentSettings,
  user,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import type { ProviderSecrets } from "@workspace/billing-web/server";
import type { PaymentMode, PaymentProviderName } from "@workspace/db/schema";

/**
 * Payment gateway management (ADMIN-03): staff choose the provider that takes
 * top-ups, its mode, and whether top-ups are on. Secrets stay in env; the
 * admin sees only whether each one is set. Every change is written with its
 * value before and after in payment_change, in the same transaction, and the
 * access middleware adds it to the admin audit log.
 */

export interface PaymentProviderSettings {
  provider: PaymentProviderName;
  enabled: boolean;
  mode: PaymentMode;
  updatedAt: Date;
  updatedBy: string | null;
}

export interface PaymentSettings {
  topupsEnabled: boolean;
  activeProvider: PaymentProviderName;
  updatedAt: Date;
  updatedBy: string | null;
  providers: PaymentProviderSettings[];
}

/** One provider as the admin page shows it. Secrets are names and states, never values. */
export interface PaymentProviderView extends PaymentProviderSettings {
  isActive: boolean;
  secrets: ProviderSecrets | null;
  /** Where the provider's dashboard must send its events. */
  webhookPath: string;
}

export interface PaymentsOverview {
  topupsEnabled: boolean;
  activeProvider: PaymentProviderName;
  updatedAt: Date;
  updatedBy: string | null;
  providers: PaymentProviderView[];
}

const SETTINGS_ID = 1;
/** As long as a change may take to reach every API instance (rule 6, SPEC.md). */
const CACHE_MS = 30_000;

let cache: { value: PaymentSettings; expiresAt: number } | null = null;

/** Forgets the cached settings: an update in this process applies at once. */
export const clearPaymentSettingsCache = () => {
  cache = null;
};

const readSettings = async (): Promise<PaymentSettings> => {
  const [[settings], providers] = await Promise.all([
    db
      .select({
        topupsEnabled: paymentSettings.topupsEnabled,
        activeProvider: paymentSettings.activeProvider,
        updatedAt: paymentSettings.updatedAt,
        updatedBy: paymentSettings.updatedBy,
      })
      .from(paymentSettings)
      .where(eq(paymentSettings.id, SETTINGS_ID)),
    db.select().from(paymentProvider).orderBy(paymentProvider.provider),
  ]);

  if (!settings) {
    // the migration creates the row and nothing deletes it
    throw new Error("payment_settings has no row: run the database migrations");
  }
  return { ...settings, providers };
};

/** The settings, at most 30 seconds old. */
export const getPaymentSettings = async (): Promise<PaymentSettings> => {
  if (cache && cache.expiresAt > Date.now()) {
    return cache.value;
  }
  const value = await readSettings();
  cache = { value, expiresAt: Date.now() + CACHE_MS };
  return value;
};

const secretsOf = (provider: string) =>
  getBillingProvider(provider)?.topUp?.secrets() ?? null;

const isModeConfigured = (provider: string, mode: PaymentMode) =>
  secretsOf(provider)?.[mode].isConfigured ?? false;

/** What the admin page shows, read fresh. */
export const getPaymentsOverview = async (): Promise<PaymentsOverview> => {
  const settings = await readSettings();
  return {
    topupsEnabled: settings.topupsEnabled,
    activeProvider: settings.activeProvider,
    updatedAt: settings.updatedAt,
    updatedBy: settings.updatedBy,
    providers: settings.providers.map((row) => ({
      ...row,
      isActive: row.provider === settings.activeProvider,
      secrets: secretsOf(row.provider),
      webhookPath: `/api/billing/webhook/${row.provider}`,
    })),
  };
};

/** The latest changes, newest first, with the name of the staff user who made each one. */
export const getPaymentChanges = async (limit = 50) =>
  db
    .select({
      id: paymentChange.id,
      target: paymentChange.target,
      before: paymentChange.before,
      after: paymentChange.after,
      changedAt: paymentChange.changedAt,
      changedBy: paymentChange.changedBy,
      changedByName: user.name,
      changedByEmail: user.email,
    })
    .from(paymentChange)
    .leftJoin(user, eq(user.id, paymentChange.changedBy))
    .orderBy(desc(paymentChange.id))
    .limit(limit);

const refuse = (code: string) =>
  new HttpException(HttpStatusCode.CONFLICT, { code });

export interface UpdatePaymentSettingsInput {
  topupsEnabled: boolean;
  activeProvider: PaymentProviderName;
  staffId: string;
}

/**
 * Top-ups on or off, and the provider new checkouts go to. The provider made
 * active must be enabled and have the secrets of its mode.
 */
export const updatePaymentSettings = async ({
  staffId,
  ...values
}: UpdatePaymentSettingsInput) => {
  const result = await db.transaction(async (tx) => {
    // lock the row so two staff saving at once are recorded in order
    const [before] = await tx
      .select({
        topupsEnabled: paymentSettings.topupsEnabled,
        activeProvider: paymentSettings.activeProvider,
      })
      .from(paymentSettings)
      .where(eq(paymentSettings.id, SETTINGS_ID))
      .for("update");
    if (!before) {
      throw new Error(
        "payment_settings has no row: run the database migrations",
      );
    }

    if (values.activeProvider !== before.activeProvider) {
      const [target] = await tx
        .select({
          enabled: paymentProvider.enabled,
          mode: paymentProvider.mode,
        })
        .from(paymentProvider)
        .where(eq(paymentProvider.provider, values.activeProvider))
        .for("update");
      if (!target?.enabled) {
        throw refuse("admin:payments.error.providerDisabled");
      }
      if (!isModeConfigured(values.activeProvider, target.mode)) {
        throw refuse("admin:payments.error.providerNotConfigured");
      }
    }

    const [after] = await tx
      .update(paymentSettings)
      .set({ ...values, updatedAt: new Date(), updatedBy: staffId })
      .where(eq(paymentSettings.id, SETTINGS_ID))
      .returning({
        topupsEnabled: paymentSettings.topupsEnabled,
        activeProvider: paymentSettings.activeProvider,
        updatedAt: paymentSettings.updatedAt,
        updatedBy: paymentSettings.updatedBy,
      });
    if (!after) {
      throw new Error("payment_settings row disappeared during the update");
    }

    await tx.insert(paymentChange).values({
      target: "settings",
      before,
      after: values,
      changedBy: staffId,
    });
    return after;
  });
  clearPaymentSettingsCache();
  return result;
};

export interface UpdatePaymentProviderInput {
  provider: PaymentProviderName;
  enabled: boolean;
  mode: PaymentMode;
  staffId: string;
}

/**
 * A provider on or off, and its mode. The active provider cannot be turned
 * off (choose another first), and a mode cannot be chosen without both of its
 * secrets.
 */
export const updatePaymentProvider = async ({
  provider,
  staffId,
  ...values
}: UpdatePaymentProviderInput): Promise<PaymentProviderSettings> => {
  const result = await db.transaction(async (tx) => {
    // the settings row first, as updatePaymentSettings locks it, so the two never deadlock
    const [settings] = await tx
      .select({ activeProvider: paymentSettings.activeProvider })
      .from(paymentSettings)
      .where(eq(paymentSettings.id, SETTINGS_ID))
      .for("update");
    const [before] = await tx
      .select({ enabled: paymentProvider.enabled, mode: paymentProvider.mode })
      .from(paymentProvider)
      .where(eq(paymentProvider.provider, provider))
      .for("update");
    if (!before) {
      throw new HttpException(HttpStatusCode.NOT_FOUND, {
        code: "error.notFound",
      });
    }

    if (!values.enabled && settings?.activeProvider === provider) {
      throw refuse("admin:payments.error.activeProviderDisabled");
    }
    if (
      values.mode !== before.mode &&
      !isModeConfigured(provider, values.mode)
    ) {
      throw refuse("admin:payments.error.secretsMissing");
    }

    const [after] = await tx
      .update(paymentProvider)
      .set({ ...values, updatedAt: new Date(), updatedBy: staffId })
      .where(eq(paymentProvider.provider, provider))
      .returning();
    if (!after) {
      throw new Error(
        `payment_provider ${provider} disappeared during the update`,
      );
    }

    await tx.insert(paymentChange).values({
      target: `provider:${provider}`,
      before,
      after: values,
      changedBy: staffId,
    });
    return after;
  });
  clearPaymentSettingsCache();
  return result;
};
