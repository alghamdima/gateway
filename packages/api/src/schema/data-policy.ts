import * as z from "zod";

import { PROVIDER_REGIONS } from "@workspace/db/schema";

/**
 * An organization's data policy (ADR 0015). A null list allows every region
 * or every provider; an empty list is refused, because it would allow none.
 */
export const setDataPolicySchema = z.object({
  requireZeroRetention: z.boolean(),
  forbidTraining: z.boolean(),
  allowedRegions: z
    .array(z.enum(PROVIDER_REGIONS))
    .min(1)
    .max(PROVIDER_REGIONS.length)
    .refine((regions) => new Set(regions).size === regions.length)
    .nullable(),
  allowedProviders: z
    .array(z.string().regex(/^[a-z0-9][a-z0-9_.-]{0,63}$/))
    .min(1)
    .max(100)
    .refine((providers) => new Set(providers).size === providers.length)
    .nullable(),
});

export type SetDataPolicyInput = z.infer<typeof setDataPolicySchema>;
