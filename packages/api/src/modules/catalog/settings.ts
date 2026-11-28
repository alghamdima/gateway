import { desc, eq } from "@workspace/db";
import {
  catalogSettings,
  catalogSettingsChange,
  user,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";

/**
 * What staff set about the catalog's figures (SPEC rule 6, CAT-11): how many
 * answered calls a figure needs before it is shown, and whether traffic
 * volume is public. One row, seeded by migration 0046; each change is
 * recorded in catalog_settings_change in the same transaction, and in the
 * admin audit log by the route.
 */

export interface CatalogSettings {
  /** A figure is shown only with at least this many answered calls behind it. */
  minCallsForStats: number;
  /** Whether a model's token volume and each endpoint's share are shown to visitors. */
  isTrafficPublic: boolean;
}

const SETTINGS_ID = 1;

const columns = {
  minCallsForStats: catalogSettings.minCallsForStats,
  isTrafficPublic: catalogSettings.isTrafficPublic,
  updatedAt: catalogSettings.updatedAt,
  updatedBy: catalogSettings.updatedBy,
};

const valuesOf = ({
  minCallsForStats,
  isTrafficPublic,
}: CatalogSettings): CatalogSettings => ({ minCallsForStats, isTrafficPublic });

const missingRow = () =>
  // the migration creates the row and nothing deletes it
  new Error("catalog_settings has no row: run the database migrations");

export const getCatalogSettings = async () => {
  const [row] = await db
    .select(columns)
    .from(catalogSettings)
    .where(eq(catalogSettings.id, SETTINGS_ID));
  if (!row) {
    throw missingRow();
  }
  return row;
};

export const updateCatalogSettings = async ({
  staffId,
  ...input
}: CatalogSettings & { staffId: string }) =>
  db.transaction(async (tx) => {
    // lock the row so two staff saving at once are recorded in order
    const [current] = await tx
      .select(columns)
      .from(catalogSettings)
      .where(eq(catalogSettings.id, SETTINGS_ID))
      .for("update");
    if (!current) {
      throw missingRow();
    }
    const [settings] = await tx
      .update(catalogSettings)
      .set({ ...valuesOf(input), updatedAt: new Date(), updatedBy: staffId })
      .where(eq(catalogSettings.id, SETTINGS_ID))
      .returning(columns);
    if (!settings) {
      throw missingRow();
    }
    const before = { ...valuesOf(current) };
    const after = { ...valuesOf(settings) };
    await tx
      .insert(catalogSettingsChange)
      .values({ before, after, changedBy: staffId });
    return { settings, before, after };
  });

/** The latest changes, newest first, with the name of the staff user who made each. */
export const getCatalogSettingsChanges = async (limit = 50) =>
  db
    .select({
      id: catalogSettingsChange.id,
      before: catalogSettingsChange.before,
      after: catalogSettingsChange.after,
      changedAt: catalogSettingsChange.changedAt,
      changedBy: catalogSettingsChange.changedBy,
      changedByName: user.name,
      changedByEmail: user.email,
    })
    .from(catalogSettingsChange)
    .leftJoin(user, eq(user.id, catalogSettingsChange.changedBy))
    .orderBy(desc(catalogSettingsChange.id))
    .limit(limit);
