import { desc, eq } from "@workspace/db";
import { siteSettings, siteSettingsChange, user } from "@workspace/db/schema";
import { db } from "@workspace/db/server";

/**
 * What staff set about the public site (SPEC rule 6, SITE-01, ADMIN-20):
 * whether the blog is served and linked, and where an email about a new
 * contact message goes. One row, seeded by migration 0047; each
 * change is recorded in site_settings_change in the same transaction, and in
 * the admin audit log by the route.
 */

export interface SiteSettings {
  /** Whether the blog's pages answer and the site links to them. */
  isBlogEnabled: boolean;
  /** Where an email about each new contact message goes; null: none is sent. */
  contactEmail: string | null;
}

const SETTINGS_ID = 1;

const columns = {
  isBlogEnabled: siteSettings.isBlogEnabled,
  contactEmail: siteSettings.contactEmail,
  updatedAt: siteSettings.updatedAt,
  updatedBy: siteSettings.updatedBy,
};

const valuesOf = ({
  isBlogEnabled,
  contactEmail,
}: SiteSettings): SiteSettings => ({ isBlogEnabled, contactEmail });

const missingRow = () =>
  // the migration creates the row and nothing deletes it
  new Error("site_settings has no row: run the database migrations");

export const getSiteSettings = async () => {
  const [row] = await db
    .select(columns)
    .from(siteSettings)
    .where(eq(siteSettings.id, SETTINGS_ID));
  if (!row) {
    throw missingRow();
  }
  return row;
};

/** What a visitor's browser may know: the switches, not the address or who set them. */
export const getPublicSiteSettings = async () => {
  const { isBlogEnabled } = await getSiteSettings();
  return { isBlogEnabled };
};

export const updateSiteSettings = async ({
  staffId,
  ...input
}: SiteSettings & { staffId: string }) =>
  db.transaction(async (tx) => {
    // lock the row so two staff saving at once are recorded in order
    const [current] = await tx
      .select(columns)
      .from(siteSettings)
      .where(eq(siteSettings.id, SETTINGS_ID))
      .for("update");
    if (!current) {
      throw missingRow();
    }
    const [settings] = await tx
      .update(siteSettings)
      .set({ ...valuesOf(input), updatedAt: new Date(), updatedBy: staffId })
      .where(eq(siteSettings.id, SETTINGS_ID))
      .returning(columns);
    if (!settings) {
      throw missingRow();
    }
    const before = { ...valuesOf(current) };
    const after = { ...valuesOf(settings) };
    await tx
      .insert(siteSettingsChange)
      .values({ before, after, changedBy: staffId });
    return { settings, before, after };
  });

/** The latest changes, newest first, with the name of the staff user who made each. */
export const getSiteSettingsChanges = async (limit = 50) =>
  db
    .select({
      id: siteSettingsChange.id,
      before: siteSettingsChange.before,
      after: siteSettingsChange.after,
      changedAt: siteSettingsChange.changedAt,
      changedBy: siteSettingsChange.changedBy,
      changedByName: user.name,
      changedByEmail: user.email,
    })
    .from(siteSettingsChange)
    .leftJoin(user, eq(user.id, siteSettingsChange.changedBy))
    .orderBy(desc(siteSettingsChange.id))
    .limit(limit);
