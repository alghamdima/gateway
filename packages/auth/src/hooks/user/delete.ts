import { APIError } from "better-auth/api";

import { isSubscriptionActive } from "@workspace/billing";
import { getCustomersWithPurchasesByReferenceId } from "@workspace/billing/server";
import { eq } from "@workspace/db";
import { member, organization } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { removeOrganizationFromGateway } from "@workspace/gateway-client/server";

import { getPersonalOrganizationId } from "../../lib/personal";
import {
  getOrganizationsBlockingAccountDeletion,
  syncSubscriptionSeats,
} from "../../lib/server";
import { DatabaseHooks } from "../types";

const organizationIdsByDeletedUser = new WeakMap<object, string[]>();
const personalOrganizationIdByDeletedUser = new WeakMap<object, string>();

/**
 * Refuses an account deletion that must not happen, and takes the personal
 * account out of the gateway (ADR 0004, ADR 0020). Run this before anything is
 * deleted: Better Auth removes the user's sessions and sign-in accounts
 * before the user row's own delete hook runs, so a refusal there would leave
 * a user who can no longer sign in. Safe to repeat.
 */
export const prepareAccountDeletion = async (userId: string) => {
  const blockingOrganizations = await getOrganizationsBlockingAccountDeletion({
    userId,
  });
  if (blockingOrganizations.length > 0) {
    throw new APIError("FORBIDDEN", {
      message:
        "You cannot delete your account because you are the sole owner of one or more organizations.",
    });
  }

  const customers = await getCustomersWithPurchasesByReferenceId(userId);
  const activeSubscriptions = customers.flatMap((customer) =>
    customer.subscriptions.filter(isSubscriptionActive),
  );
  if (activeSubscriptions.length > 0) {
    throw new APIError("FORBIDDEN", {
      message:
        "You cannot delete your account because you have active subscriptions.",
    });
  }

  // the personal account goes with its user: its keys and customer leave the
  // gateway first, and a gateway failure stops the deletion, as it does for
  // an organization
  const personalOrganizationId = await getPersonalOrganizationId(userId);
  if (personalOrganizationId) {
    await removeOrganizationFromGateway(personalOrganizationId);
  }
  return { personalOrganizationId };
};

export const deleteUser = {
  before: async (user) => {
    // the last net: every known way to delete a user has already run this
    const { personalOrganizationId } = await prepareAccountDeletion(user.id);
    if (personalOrganizationId) {
      personalOrganizationIdByDeletedUser.set(user, personalOrganizationId);
    }

    const memberships = await db
      .select({ organizationId: member.organizationId })
      .from(member)
      .where(eq(member.userId, user.id));

    organizationIdsByDeletedUser.set(user, [
      ...new Set(
        memberships
          .map((membership) => membership.organizationId)
          .filter((id) => id !== personalOrganizationId),
      ),
    ]);
  },
  after: async (user) => {
    const organizationIds = organizationIdsByDeletedUser.get(user);
    organizationIdsByDeletedUser.delete(user);

    const personalOrganizationId =
      personalOrganizationIdByDeletedUser.get(user);
    personalOrganizationIdByDeletedUser.delete(user);
    if (personalOrganizationId) {
      await db
        .delete(organization)
        .where(eq(organization.id, personalOrganizationId));
    }

    await Promise.all(
      organizationIds?.map((organizationId) =>
        syncSubscriptionSeats(organizationId),
      ) ?? [],
    );
  },
} satisfies NonNullable<DatabaseHooks["user"]>["delete"];
