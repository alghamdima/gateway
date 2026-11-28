import { completeOnboarding, refuseIfPersonal } from "../../lib/personal";
import { syncSubscriptionSeats } from "../../lib/server";
import { OrganizationHooks } from "./types";

export const { beforeAddMember, afterAddMember, afterAcceptInvitation } = {
  beforeAddMember: async ({ organization }) => {
    await refuseIfPersonal(organization.id);
    /**
     * Here you can enforce any business logic before adding a member to the organization.
     * For example, for one-time per-seat purchase you can check if the organization has any
     * seats left and if not force the owner/admin to purchase more seats.
     */
  },
  afterAddMember: async ({ organization }) => {
    await syncSubscriptionSeats(organization.id);
  },
  afterAcceptInvitation: async ({ organization, user }) => {
    // someone who joins by invitation is there for the organization and is
    // not shown the welcome wizard (ONB-13)
    await completeOnboarding(user.id, "organization");
    await syncSubscriptionSeats(organization.id);
  },
} satisfies OrganizationHooks;
