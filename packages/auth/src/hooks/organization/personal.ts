import {
  refuseIfPersonal,
  refuseOverOrganizationLimit,
} from "../../lib/personal";
import { OrganizationHooks } from "./types";

/**
 * A personal account is closed to other people and is not managed as an
 * organization (ADR 0020): no invitations, no rename, no role changes. Adding
 * a member and deleting it are refused in their own hook files.
 */
export const {
  beforeCreateOrganization,
  beforeCreateInvitation,
  beforeAcceptInvitation,
  beforeUpdateOrganization,
  beforeUpdateMemberRole,
} = {
  // a user owns only so many organizations (a staff setting, ADR 0020)
  beforeCreateOrganization: async ({ user }) => {
    await refuseOverOrganizationLimit(user.id);
  },
  beforeCreateInvitation: async ({ organization }) => {
    await refuseIfPersonal(organization.id);
  },
  beforeAcceptInvitation: async ({ organization }) => {
    await refuseIfPersonal(organization.id);
  },
  // `organization` here is only the new values; the member names the account
  beforeUpdateOrganization: async ({ member }) => {
    await refuseIfPersonal(member.organizationId);
  },
  beforeUpdateMemberRole: async ({ organization }) => {
    await refuseIfPersonal(organization.id);
  },
} satisfies OrganizationHooks;
