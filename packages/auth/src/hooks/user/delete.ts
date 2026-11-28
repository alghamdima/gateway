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

export const deleteUser = {
  before: async (user) => {
    const blockingOrganizations = await getOrganizationsBlockingAccountDeletion(
      { userId: user.id },
    );

    if (blockingOrganizations.length > 0) {
      throw new Error(
        "You cannot delete your account because you are the sole owner of one or more organizations.",
      );
    }

    const customers = await getCustomersWithPurchasesByReferenceId(user.id);
    const activeSubscriptions = customers.flatMap((customer) =>
      customer.subscriptions.filter(isSubscriptionActive),
    );

    if (activeSubscriptions.length > 0) {
      throw new Error(
        "You cannot delete your account because you have active subscriptions.",
      );
    }

    const memberships = await db
      .select({ organizationId: member.organizationId })
      .from(member)
      .where(eq(member.userId, user.id));

    // the personal account goes with its user (ADR 0020): its keys and
    // customer leave the gateway first (ADR 0004), and a gateway failure
    // stops the deletion, as it does for an organization
    const personalOrganizationId = await getPersonalOrganizationId(user.id);
    if (personalOrganizationId) {
      await removeOrganizationFromGateway(personalOrganizationId);
      personalOrganizationIdByDeletedUser.set(user, personalOrganizationId);
    }

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
