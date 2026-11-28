import {
  countOwnedOrganizations,
  getOwnedOrganizationLimit,
} from "@workspace/auth/server";
import { desc, eq } from "@workspace/db";
import {
  accountSettings,
  accountSettingsChange,
  user,
  userOrganizationLimit,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

/**
 * What staff set about accounts (ADR 0020): how many organizations a user
 * may own, for everyone and for one user. The general value is one row,
 * seeded by migration 0043; each change is recorded in
 * account_settings_change in the same transaction, and in the admin audit log
 * by the route. The limit itself is enforced where an organization is
 * created (packages/auth/src/lib/personal.ts).
 */

export interface AccountSettings {
  maxOwnedOrganizations: number;
}

const SETTINGS_ID = 1;

const columns = {
  maxOwnedOrganizations: accountSettings.maxOwnedOrganizations,
  updatedAt: accountSettings.updatedAt,
  updatedBy: accountSettings.updatedBy,
};

const missingRow = () =>
  // the migration creates the row and nothing deletes it
  new Error("account_settings has no row: run the database migrations");

export const getAccountSettings = async () => {
  const [row] = await db
    .select(columns)
    .from(accountSettings)
    .where(eq(accountSettings.id, SETTINGS_ID));
  if (!row) {
    throw missingRow();
  }
  return row;
};

export const updateAccountSettings = async ({
  staffId,
  maxOwnedOrganizations,
}: AccountSettings & { staffId: string }) =>
  db.transaction(async (tx) => {
    // lock the row so two staff saving at once are recorded in order
    const [current] = await tx
      .select(columns)
      .from(accountSettings)
      .where(eq(accountSettings.id, SETTINGS_ID))
      .for("update");
    if (!current) {
      throw missingRow();
    }
    const [settings] = await tx
      .update(accountSettings)
      .set({ maxOwnedOrganizations, updatedAt: new Date(), updatedBy: staffId })
      .where(eq(accountSettings.id, SETTINGS_ID))
      .returning(columns);
    if (!settings) {
      throw missingRow();
    }
    const before = { maxOwnedOrganizations: current.maxOwnedOrganizations };
    const after = { maxOwnedOrganizations: settings.maxOwnedOrganizations };
    await tx
      .insert(accountSettingsChange)
      .values({ before, after, changedBy: staffId });
    return { settings, before, after };
  });

/** The latest changes, newest first, with the name of the staff user who made each. */
export const getAccountSettingsChanges = async (limit = 50) =>
  db
    .select({
      id: accountSettingsChange.id,
      before: accountSettingsChange.before,
      after: accountSettingsChange.after,
      changedAt: accountSettingsChange.changedAt,
      changedBy: accountSettingsChange.changedBy,
      changedByName: user.name,
      changedByEmail: user.email,
    })
    .from(accountSettingsChange)
    .leftJoin(user, eq(user.id, accountSettingsChange.changedBy))
    .orderBy(desc(accountSettingsChange.id))
    .limit(limit);

const ownLimit = async (userId: string) =>
  db
    .select({ max: userOrganizationLimit.maxOwnedOrganizations })
    .from(userOrganizationLimit)
    .where(eq(userOrganizationLimit.userId, userId))
    .then((rows) => rows[0]?.max ?? null);

/** One user's limit as staff see it: their own value if set, what applies, and what they own. */
export const getUserOrganizationLimit = async (userId: string) => {
  const [own, effective, owned] = await Promise.all([
    ownLimit(userId),
    getOwnedOrganizationLimit(userId),
    countOwnedOrganizations(userId),
  ]);
  return { own, effective, owned };
};

/** Sets one user's own limit; null removes it, so the general limit applies again. */
export const setUserOrganizationLimit = async ({
  userId,
  staffId,
  maxOwnedOrganizations,
}: {
  userId: string;
  staffId: string;
  maxOwnedOrganizations: number | null;
}) => {
  const [target] = await db
    .select({ id: user.id })
    .from(user)
    .where(eq(user.id, userId));
  if (!target) {
    throw new HttpException(HttpStatusCode.NOT_FOUND, {
      message: "No such user",
    });
  }
  const before = { maxOwnedOrganizations: await ownLimit(userId) };
  await (maxOwnedOrganizations === null
    ? db
        .delete(userOrganizationLimit)
        .where(eq(userOrganizationLimit.userId, userId))
    : db
        .insert(userOrganizationLimit)
        .values({ userId, maxOwnedOrganizations, updatedBy: staffId })
        .onConflictDoUpdate({
          target: userOrganizationLimit.userId,
          set: {
            maxOwnedOrganizations,
            updatedAt: new Date(),
            updatedBy: staffId,
          },
        }));
  return {
    limit: await getUserOrganizationLimit(userId),
    before,
    after: { maxOwnedOrganizations },
  };
};
