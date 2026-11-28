import * as z from "zod";

import { MAX_OWNED_ORGANIZATIONS_CEILING } from "@workspace/db/schema";

export { MAX_OWNED_ORGANIZATIONS_CEILING };

const ownedOrganizationsSchema = z
  .number()
  .int()
  .min(0)
  .max(MAX_OWNED_ORGANIZATIONS_CEILING);

/** How many organizations a user may own (ADR 0020). */
export const updateAccountSettingsSchema = z.object({
  maxOwnedOrganizations: ownedOrganizationsSchema,
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
