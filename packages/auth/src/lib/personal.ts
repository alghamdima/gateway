import { APIError } from "better-auth/api";
import { randomUUID } from "node:crypto";

import { and, count, eq, isNull, sql } from "@workspace/db";
import {
  accountSettings,
  member,
  organization,
  personalAccount,
  userOrganizationLimit,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";

import { MemberRole } from "../types";

import type { AccountType } from "@workspace/db/schema";

/**
 * Personal accounts (ADR 0020). A personal account is an organization row
 * that `personal_account` names: credits, keys and the gateway treat it as
 * any other account. It has one member, its user, and is closed to everyone
 * else; the customer sees it as "Personal", never as an organization.
 */

export const PERSONAL_ACCOUNT_NAME = "Personal";

// the same shape the 0042 backfill gives existing users' accounts
const newId = () => randomUUID().replaceAll("-", "");

/** Creates the user's personal account; does nothing when they have one. */
export const createPersonalAccount = async (userId: string) =>
  db.transaction(async (tx) => {
    const existing = await tx.query.personalAccount.findFirst({
      where: eq(personalAccount.userId, userId),
    });
    if (existing) {
      return existing.organizationId;
    }
    const organizationId = newId();
    const now = new Date();
    await tx.insert(organization).values({
      id: organizationId,
      name: PERSONAL_ACCOUNT_NAME,
      slug: `personal-${organizationId}`,
      createdAt: now,
    });
    await tx.insert(member).values({
      id: newId(),
      organizationId,
      userId,
      role: MemberRole.OWNER,
      createdAt: now,
    });
    await tx.insert(personalAccount).values({ userId, organizationId });
    return organizationId;
  });

export const isPersonalOrganization = async (organizationId: string) =>
  db
    .select({ userId: personalAccount.userId })
    .from(personalAccount)
    .where(eq(personalAccount.organizationId, organizationId))
    .limit(1)
    .then((rows) => rows.length > 0);

export const getPersonalOrganizationId = async (userId: string) =>
  db
    .select({ organizationId: personalAccount.organizationId })
    .from(personalAccount)
    .where(eq(personalAccount.userId, userId))
    .limit(1)
    .then((rows) => rows[0]?.organizationId ?? null);

/**
 * The user's personal account as the app addresses it: by its organization id
 * and by the slug that is its place in the URL. Creates it when it is missing,
 * so a user always has one to land on.
 */
export const getPersonalAccount = async (userId: string) => {
  const organizationId = await createPersonalAccount(userId);
  const [row] = await db
    .select({
      id: organization.id,
      slug: organization.slug,
      // null: the welcome wizard is still to be shown (ONB-03)
      onboardedAt: personalAccount.onboardedAt,
    })
    .from(organization)
    .innerJoin(
      personalAccount,
      eq(personalAccount.organizationId, organization.id),
    )
    .where(eq(organization.id, organizationId));
  if (!row) {
    throw new Error("The personal account has no organization row");
  }
  return row;
};

/** What every refused change to a personal account says. */
export const PERSONAL_ACCOUNT_CLOSED_MESSAGE =
  "A personal account has one member, its owner, and cannot be changed or deleted on its own";

/** Refuses a membership, rename or deletion aimed at a personal account. */
export const refuseIfPersonal = async (organizationId: string) => {
  if (await isPersonalOrganization(organizationId)) {
    // an APIError, so the customer is shown the reason
    throw new APIError("FORBIDDEN", {
      message: PERSONAL_ACCOUNT_CLOSED_MESSAGE,
    });
  }
};

/**
 * How many organizations the user may own (ADR 0020): their own limit when
 * staff set one, otherwise the general one. The personal account is not an
 * organization and is not counted.
 */
export const getOwnedOrganizationLimit = async (userId: string) => {
  const [own] = await db
    .select({ max: userOrganizationLimit.maxOwnedOrganizations })
    .from(userOrganizationLimit)
    .where(eq(userOrganizationLimit.userId, userId));
  if (own) {
    return own.max;
  }
  const [general] = await db
    .select({ max: accountSettings.maxOwnedOrganizations })
    .from(accountSettings);
  if (!general) {
    // the migration creates the row and nothing deletes it
    throw new Error("account_settings has no row: run the database migrations");
  }
  return general.max;
};

export const countOwnedOrganizations = async (userId: string) =>
  db
    .select({ count: count() })
    .from(member)
    .where(
      and(
        eq(member.userId, userId),
        eq(member.role, MemberRole.OWNER),
        sql`not exists (
          select 1 from ${personalAccount}
          where ${personalAccount.organizationId} = ${member.organizationId}
        )`,
      ),
    )
    .then((rows) => rows[0]?.count ?? 0);

/** What a user at their limit is told when they try to create another. */
export const organizationLimitMessage = (limit: number) =>
  `You own the most organizations an account may own (${limit}). Contact support to raise the limit.`;

/** Refuses a new organization for a user who already owns as many as they may. */
export const refuseOverOrganizationLimit = async (userId: string) => {
  const [limit, owned] = await Promise.all([
    getOwnedOrganizationLimit(userId),
    countOwnedOrganizations(userId),
  ]);
  if (owned >= limit) {
    throw new APIError("FORBIDDEN", {
      message: organizationLimitMessage(limit),
    });
  }
};

/**
 * Ends the user's onboarding (ONB-03): the wizard was finished, or they
 * joined an organization by invitation and skip it (ONB-13). Does nothing
 * when it has ended already, so the first answer stays.
 */
export const completeOnboarding = async (
  userId: string,
  accountType: AccountType,
) => {
  await createPersonalAccount(userId);
  await db
    .update(personalAccount)
    .set({ onboardedAt: new Date(), accountType })
    .where(
      and(
        eq(personalAccount.userId, userId),
        isNull(personalAccount.onboardedAt),
      ),
    );
};
