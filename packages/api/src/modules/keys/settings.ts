import { desc, eq } from "@workspace/db";
import { keySettings, keySettingsChange, user } from "@workspace/db/schema";
import { db } from "@workspace/db/server";

import {
  choiceFromSeconds,
  expiryFromSeconds,
  secondsFromChoice,
} from "./expiry";

import type { ExpiryChoice } from "./expiry";
import type { ApiKeyLimitReset, KeyExpiry } from "@workspace/db/schema";

/**
 * The key defaults staff set in the admin (ADMIN-12): what the create form
 * starts from and how long any key may live. One row, seeded by migration
 * 0040; each change is recorded in key_settings_change in the same
 * transaction, and in the admin audit log by the route.
 */

export interface KeySettings {
  /** micro-SAR; null: new keys have no limit by default. */
  defaultLimitMicroSar: number | null;
  defaultLimitReset: ApiKeyLimitReset | null;
  defaultExpiry: ExpiryChoice;
  /** null: no maximum. */
  maxLifetime: KeyExpiry | null;
  /** micro-SAR, smallest first. */
  limitPresetsMicroSar: number[];
}

export interface KeySettingsRecord extends KeySettings {
  /** Seconds, as stored: what the expiry check and the lifetime job use. */
  maxLifetimeSeconds: number | null;
  updatedAt: Date;
  updatedBy: string | null;
}

const SETTINGS_ID = 1;

const columns = {
  defaultLimitMicroSar: keySettings.defaultLimitMicroSar,
  defaultLimitReset: keySettings.defaultLimitReset,
  defaultExpirySeconds: keySettings.defaultExpirySeconds,
  maxLifetimeSeconds: keySettings.maxLifetimeSeconds,
  limitPresetsMicroSar: keySettings.limitPresetsMicroSar,
  updatedAt: keySettings.updatedAt,
  updatedBy: keySettings.updatedBy,
};

interface Row {
  defaultLimitMicroSar: number | null;
  defaultLimitReset: ApiKeyLimitReset | null;
  defaultExpirySeconds: number | null;
  maxLifetimeSeconds: number | null;
  limitPresetsMicroSar: number[];
  updatedAt: Date;
  updatedBy: string | null;
}

const fromRow = (row: Row): KeySettingsRecord => ({
  defaultLimitMicroSar: row.defaultLimitMicroSar,
  defaultLimitReset: row.defaultLimitReset,
  defaultExpiry: choiceFromSeconds(row.defaultExpirySeconds),
  maxLifetime:
    row.maxLifetimeSeconds === null
      ? null
      : expiryFromSeconds(row.maxLifetimeSeconds),
  limitPresetsMicroSar: row.limitPresetsMicroSar,
  maxLifetimeSeconds: row.maxLifetimeSeconds,
  updatedAt: row.updatedAt,
  updatedBy: row.updatedBy,
});

/** The values a change records before and after, without who and when. */
const valuesOf = ({
  defaultLimitMicroSar,
  defaultLimitReset,
  defaultExpiry,
  maxLifetime,
  limitPresetsMicroSar,
}: KeySettings): KeySettings => ({
  defaultLimitMicroSar,
  defaultLimitReset,
  defaultExpiry,
  maxLifetime,
  limitPresetsMicroSar,
});

export const getKeySettings = async (): Promise<KeySettingsRecord> => {
  const [row] = await db
    .select(columns)
    .from(keySettings)
    .where(eq(keySettings.id, SETTINGS_ID));
  if (!row) {
    // the migration creates the row and nothing deletes it
    throw new Error("key_settings has no row: run the database migrations");
  }
  return fromRow(row);
};

export interface KeySettingsChange {
  settings: KeySettingsRecord;
  before: KeySettings;
  after: KeySettings;
}

/**
 * Saves the defaults and records the change. Shortening the maximum lifetime
 * is applied to existing keys by the caller afterwards (lifetime.ts), outside
 * this transaction: it calls the gateway.
 */
export const updateKeySettings = async ({
  staffId,
  ...input
}: KeySettings & { staffId: string }): Promise<KeySettingsChange> =>
  db.transaction(async (tx) => {
    // lock the row so two staff saving at once are recorded in order
    const [current] = await tx
      .select(columns)
      .from(keySettings)
      .where(eq(keySettings.id, SETTINGS_ID))
      .for("update");
    if (!current) {
      throw new Error("key_settings has no row: run the database migrations");
    }
    const [updated] = await tx
      .update(keySettings)
      .set({
        defaultLimitMicroSar: input.defaultLimitMicroSar,
        defaultLimitReset:
          input.defaultLimitMicroSar === null ? null : input.defaultLimitReset,
        defaultExpirySeconds: secondsFromChoice(input.defaultExpiry),
        maxLifetimeSeconds: secondsFromChoice(input.maxLifetime ?? "never"),
        // each quick pick once, smallest first, whatever staff typed
        limitPresetsMicroSar: [...new Set(input.limitPresetsMicroSar)].sort(
          (a, b) => a - b,
        ),
        updatedAt: new Date(),
        updatedBy: staffId,
      })
      .where(eq(keySettings.id, SETTINGS_ID))
      .returning(columns);
    if (!updated) {
      throw new Error("key_settings row disappeared during the update");
    }
    const settings = fromRow(updated);
    const change = {
      before: valuesOf(fromRow(current)),
      after: valuesOf(settings),
    };
    await tx.insert(keySettingsChange).values({
      before: { ...change.before },
      after: { ...change.after },
      changedBy: staffId,
    });
    return { settings, ...change };
  });

/** The latest changes, newest first, with the name of the staff user who made each. */
export const getKeySettingsChanges = async (limit = 50) =>
  db
    .select({
      id: keySettingsChange.id,
      before: keySettingsChange.before,
      after: keySettingsChange.after,
      changedAt: keySettingsChange.changedAt,
      changedBy: keySettingsChange.changedBy,
      changedByName: user.name,
      changedByEmail: user.email,
    })
    .from(keySettingsChange)
    .leftJoin(user, eq(user.id, keySettingsChange.changedBy))
    .orderBy(desc(keySettingsChange.id))
    .limit(limit);
