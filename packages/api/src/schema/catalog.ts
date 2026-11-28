import * as z from "zod";

import { MAX_MIN_CALLS_FOR_STATS } from "@workspace/db/schema";

export { MAX_MIN_CALLS_FOR_STATS };

/** What staff set about the catalog's figures (CAT-11). */
export const updateCatalogSettingsSchema = z.object({
  minCallsForStats: z.number().int().min(1).max(MAX_MIN_CALLS_FOR_STATS),
  isTrafficPublic: z.boolean(),
});

export type UpdateCatalogSettingsInput = z.infer<
  typeof updateCatalogSettingsSchema
>;
