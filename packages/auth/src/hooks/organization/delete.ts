import { isSubscriptionActive } from "@workspace/billing";
import { getCustomersWithPurchasesByReferenceId } from "@workspace/billing/server";
import { removeOrganizationFromGateway } from "@workspace/gateway-client/server";

import { refuseIfPersonal } from "../../lib/personal";
import { OrganizationHooks } from "./types";

export const { beforeDeleteOrganization } = {
  beforeDeleteOrganization: async ({ organization }) => {
    await refuseIfPersonal(organization.id);

    const customers = await getCustomersWithPurchasesByReferenceId(
      organization.id,
    );
    const activeSubscriptions = customers.flatMap((customer) =>
      customer.subscriptions.filter(isSubscriptionActive),
    );

    if (activeSubscriptions.length > 0) {
      throw new Error(
        "You cannot delete this organization because it has active subscriptions.",
      );
    }

    await removeOrganizationFromGateway(organization.id);
  },
} satisfies OrganizationHooks;
