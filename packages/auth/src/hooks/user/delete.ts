import { isSubscriptionActive } from "@workspace/billing";
import { getCustomersWithPurchasesByReferenceId } from "@workspace/billing/server";
import { eq } from "@workspace/db";
import { member } from "@workspace/db/schema";
import { db } from "@workspace/db/server";

import {
  getOrganizationsBlockingAccountDeletion,
  syncSubscriptionSeats,
} from "../../lib/server";
import { DatabaseHooks } from "../types";

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

    if (!memberships.length) {
      return;
    }

    const organizationIds = [
      ...new Set(memberships.map((m) => m.organizationId)),
    ];

    await db.delete(member).where(eq(member.userId, user.id));
    await Promise.all(organizationIds.map(syncSubscriptionSeats));
  },
} satisfies NonNullable<DatabaseHooks["user"]>["delete"];
