import * as z from "zod";

import {
  MAX_OWNED_ORGANIZATIONS_CEILING,
  MAX_TRANSFER_WAIT_HOURS,
} from "@workspace/db/schema";

export { MAX_OWNED_ORGANIZATIONS_CEILING, MAX_TRANSFER_WAIT_HOURS };

const ownedOrganizationsSchema = z
  .number()
  .int()
  .min(0)
  .max(MAX_OWNED_ORGANIZATIONS_CEILING);

/** How many organizations a user may own, and the waits on credit transfers (ADR 0020). */
const transferWaitHoursSchema = z
  .number()
  .int()
  .min(0)
  .max(MAX_TRANSFER_WAIT_HOURS);

export const updateAccountSettingsSchema = z.object({
  maxOwnedOrganizations: ownedOrganizationsSchema,
  /** Credit transfers (BILL-12): the two waits, in hours; 0 is no wait. */
  transferMemberTenureHours: transferWaitHoursSchema,
  transferCooldownHours: transferWaitHoursSchema,
});

/** One user's own limit; null: the general limit applies. */
export const setUserOrganizationLimitSchema = z.object({
  maxOwnedOrganizations: ownedOrganizationsSchema.nullable(),
});

export type UpdateAccountSettingsInput = z.infer<
  typeof updateAccountSettingsSchema
>;
export type SetUserOrganizationLimitInput = z.infer<
  typeof setUserOrganizationLimitSchema
>;
