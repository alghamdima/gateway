import * as z from "zod";

import {
  InvitationStatus,
  MemberRole,
  SocialProvider,
  UserRole,
} from "@workspace/auth";
import { PaymentStatus, SubscriptionStatus } from "@workspace/billing";
import {
  MAX_MARKUP_BPS,
  MAX_TOPUP_FEE_BPS,
  MAX_TOPUP_FEE_MIN_MICRO_SAR,
  MAX_TOPUP_MICRO_SAR,
  MAX_TOPUP_PRESETS,
  MAX_FX_SPREAD_BPS,
  MAX_USD_TO_SAR_RATE_E6,
  MIN_USD_TO_SAR_RATE_E6,
  PAYMENT_MODES,
  PAYMENT_PROVIDERS,
  PROVIDER_AGREEMENTS,
  PROVIDER_REGIONS,
  selectCustomerSchema,
  selectOrderSchema,
  selectSubscriptionSchema,
} from "@workspace/db/schema";
import { offsetPaginationSchema } from "@workspace/shared/schema";

import {
  sortQuerySchema,
  multiValueQuerySchema,
  rangeQuerySchema,
  booleanQuerySchema,
} from "./shared";

export const getUsersInputSchema = offsetPaginationSchema.extend({
  sort: sortQuerySchema.optional(),
  q: z.string().optional(),
  role: multiValueQuerySchema(z.enum(UserRole)).optional(),
  twoFactorEnabled: multiValueQuerySchema(booleanQuerySchema).optional(),
  banned: multiValueQuerySchema(booleanQuerySchema).optional(),
  createdAt: rangeQuerySchema.optional(),
});

export type GetUsersInput = z.infer<typeof getUsersInputSchema>;

export const getUsersResponseSchema = z.object({
  data: z.array(
    z.object({
      id: z.string(),
      createdAt: z.coerce.date(),
      updatedAt: z.coerce.date(),
      email: z.string(),
      emailVerified: z.boolean(),
      name: z.string(),
      image: z.string().nullish(),
      twoFactorEnabled: z.boolean().nullable(),
      isAnonymous: z.boolean(),
      banned: z.boolean().nullable(),
      role: z.string().nullish(),
      banReason: z.string().nullish(),
      banExpires: z.coerce.date().nullish(),
    }),
  ),
  total: z.number(),
});

export type GetUsersResponse = z.infer<typeof getUsersResponseSchema>;

export const getUserAccountsInputSchema = offsetPaginationSchema.extend({
  sort: sortQuerySchema.optional(),
  providerId: multiValueQuerySchema(
    z.enum(["credential", ...Object.values(SocialProvider)]),
  ).optional(),
  createdAt: rangeQuerySchema.optional(),
  updatedAt: rangeQuerySchema.optional(),
});

export type GetUserAccountsInput = z.infer<typeof getUserAccountsInputSchema>;

export const getUserSubscriptionsInputSchema = offsetPaginationSchema.extend({
  sort: sortQuerySchema.optional(),
  status: multiValueQuerySchema(z.enum(SubscriptionStatus)).optional(),
  createdAt: rangeQuerySchema.optional(),
});

export type GetUserSubscriptionsInput = z.infer<
  typeof getUserSubscriptionsInputSchema
>;

export const getUserSubscriptionsResponseSchema = z.object({
  data: z.array(selectSubscriptionSchema),
  total: z.number(),
});

export type GetUserSubscriptionsResponse = z.infer<
  typeof getUserSubscriptionsResponseSchema
>;

export const getUserOrdersInputSchema = offsetPaginationSchema.extend({
  sort: sortQuerySchema.optional(),
  status: multiValueQuerySchema(z.enum(PaymentStatus)).optional(),
  store: multiValueQuerySchema(z.string()).optional(),
  createdAt: rangeQuerySchema.optional(),
});
export type GetUserOrdersInput = z.infer<typeof getUserOrdersInputSchema>;

export const getUserOrdersResponseSchema = z.object({
  data: z.array(selectOrderSchema),
  total: z.number(),
});
export type GetUserOrdersResponse = z.infer<typeof getUserOrdersResponseSchema>;

export const getUserAccountsResponseSchema = z.object({
  data: z.array(
    z.object({
      id: z.string(),
      userId: z.string(),
      providerId: z.string(),
      accountId: z.string(),
      createdAt: z.coerce.date(),
      updatedAt: z.coerce.date(),
    }),
  ),
  total: z.number(),
});

export type GetUserAccountsResponse = z.infer<
  typeof getUserAccountsResponseSchema
>;

export const getUserMembershipsInputSchema = offsetPaginationSchema.extend({
  sort: sortQuerySchema.optional(),
  role: multiValueQuerySchema(z.enum(MemberRole)).optional(),
  createdAt: rangeQuerySchema.optional(),
});

export type GetUserMembershipsInput = z.infer<
  typeof getUserMembershipsInputSchema
>;

export const getUserMembershipsResponseSchema = z.object({
  data: z.array(
    z.object({
      id: z.string(),
      organizationId: z.string(),
      role: z.enum(MemberRole),
      createdAt: z.coerce.date(),
      userId: z.string(),
      organization: z.object({
        id: z.string(),
        name: z.string(),
        slug: z.string().nullish(),
        logo: z.string().nullish(),
      }),
      user: z.object({
        id: z.string(),
        name: z.string(),
        email: z.string(),
      }),
    }),
  ),
  total: z.number(),
});

export type GetUserMembershipsResponse = z.infer<
  typeof getUserMembershipsResponseSchema
>;

export const getUserInvitationsInputSchema = offsetPaginationSchema.extend({
  sort: sortQuerySchema.optional(),
  role: multiValueQuerySchema(z.enum(MemberRole)).optional(),
  status: multiValueQuerySchema(z.enum(InvitationStatus)).optional(),
  expiresAt: rangeQuerySchema.optional(),
});

export type GetUserInvitationsInput = z.infer<
  typeof getUserInvitationsInputSchema
>;

export const getUserInvitationsResponseSchema = z.object({
  data: z.array(
    z.object({
      id: z.string(),
      email: z.string(),
      role: z.enum(MemberRole),
      status: z.enum(InvitationStatus),
      expiresAt: z.coerce.date(),
      createdAt: z.coerce.date(),
      inviterId: z.string(),
      organizationId: z.string(),
      organization: z.object({
        id: z.string(),
        name: z.string(),
        logo: z.string().nullish(),
      }),
    }),
  ),
  total: z.number(),
});

export type GetUserInvitationsResponse = z.infer<
  typeof getUserInvitationsResponseSchema
>;

export const getOrganizationsInputSchema = offsetPaginationSchema.extend({
  sort: sortQuerySchema.optional(),
  q: z.string().optional(),
  createdAt: rangeQuerySchema.optional(),
  members: rangeQuerySchema.optional(),
});

export type GetOrganizationsInput = z.infer<typeof getOrganizationsInputSchema>;

export const getOrganizationsResponseSchema = z.object({
  data: z.array(
    z.object({
      id: z.string(),
      name: z.string(),
      slug: z.string().nullish(),
      logo: z.string().nullish(),
      createdAt: z.coerce.date(),
      members: z.number(),
    }),
  ),
  total: z.number(),
  max: z.object({
    members: z.number(),
  }),
});

export type GetOrganizationsResponse = z.infer<
  typeof getOrganizationsResponseSchema
>;

export const getOrganizationResponseSchema = z
  .object({
    id: z.string(),
    name: z.string(),
    slug: z.string().nullish(),
    logo: z.string().nullish(),
    createdAt: z.coerce.date(),
  })
  .nullable();

export type GetOrganizationResponse = z.infer<
  typeof getOrganizationResponseSchema
>;

export const getOrganizationSubscriptionsInputSchema =
  offsetPaginationSchema.extend({
    sort: sortQuerySchema.optional(),
    status: multiValueQuerySchema(z.enum(SubscriptionStatus)).optional(),
    createdAt: rangeQuerySchema.optional(),
  });

export type GetOrganizationSubscriptionsInput = z.infer<
  typeof getOrganizationSubscriptionsInputSchema
>;

export const getOrganizationSubscriptionsResponseSchema = z.object({
  data: z.array(selectSubscriptionSchema),
  total: z.number(),
});

export type GetOrganizationSubscriptionsResponse = z.infer<
  typeof getOrganizationSubscriptionsResponseSchema
>;

export const getOrganizationOrdersInputSchema = offsetPaginationSchema.extend({
  sort: sortQuerySchema.optional(),
  status: multiValueQuerySchema(z.enum(PaymentStatus)).optional(),
  store: multiValueQuerySchema(z.string()).optional(),
  createdAt: rangeQuerySchema.optional(),
});

export type GetOrganizationOrdersInput = z.infer<
  typeof getOrganizationOrdersInputSchema
>;

export const getOrganizationOrdersResponseSchema = z.object({
  data: z.array(selectOrderSchema),
  total: z.number(),
});

export type GetOrganizationOrdersResponse = z.infer<
  typeof getOrganizationOrdersResponseSchema
>;

export const getCustomersInputSchema = offsetPaginationSchema.extend({
  sort: sortQuerySchema.optional(),
  q: z.string().optional(),
  provider: multiValueQuerySchema(z.string()).optional(),
  subscriptions: rangeQuerySchema.optional(),
  orders: rangeQuerySchema.optional(),
  createdAt: rangeQuerySchema.optional(),
});

export type GetCustomersInput = z.infer<typeof getCustomersInputSchema>;

export const getCustomersResponseSchema = z.object({
  data: z.array(
    selectCustomerSchema.extend({
      user: z
        .object({
          name: z.string(),
          image: z.string().nullish(),
        })
        .nullable(),
      organization: z
        .object({
          name: z.string(),
          logo: z.string().nullish(),
        })
        .nullable(),
      subscriptions: z.number(),
      orders: z.number(),
    }),
  ),
  total: z.number(),
  max: z.object({
    subscriptions: z.number(),
    orders: z.number(),
  }),
  options: z.object({
    provider: z.array(z.string()),
  }),
});

export type GetCustomersResponse = z.infer<typeof getCustomersResponseSchema>;

const perMinuteSchema = (max: number) =>
  z.number().int().positive().max(max).nullable();

/** Both null removes the organization's rate limit. */
export const setOrganizationRateLimitSchema = z.object({
  requestsPerMinute: perMinuteSchema(1_000_000),
  tokensPerMinute: perMinuteSchema(10_000_000_000),
});

export const adjustOrganizationCreditsSchema = z.object({
  /** Signed micro-SAR (1 SAR = 1,000,000): positive adds credit, negative removes it. */
  amount: z
    .number()
    .int()
    .refine((value) => value !== 0),
  reason: z.string().trim().min(3).max(500),
  /** Send the same key when retrying, so one decision is never applied twice. */
  idempotencyKey: z.string().min(8).max(200).optional(),
});

export type AdjustOrganizationCreditsInput = z.infer<
  typeof adjustOrganizationCreditsSchema
>;

const markupBpsSchema = z.number().int().min(0).max(MAX_MARKUP_BPS);

/** The admin form checks the same ceilings before sending. */
export {
  MAX_FX_SPREAD_BPS,
  MAX_TOPUP_MICRO_SAR,
  MAX_TOPUP_PRESETS,
  MAX_USD_TO_SAR_RATE_E6,
  MIN_USD_TO_SAR_RATE_E6,
};

/** A top-up amount in micro-SAR: whole halalas, above zero, under the typo ceiling. */
const topupAmountMicroSarSchema = z
  .number()
  .int()
  .positive()
  .max(MAX_TOPUP_MICRO_SAR)
  .refine((value) => value % 10_000 === 0, {
    message: "Must be a whole number of halalas",
  });

/** The same rules as the database checks on pricing_settings (packages/db/src/schema/pricing.ts). */
export const updatePricingSettingsSchema = z
  .object({
    /** Basis points added to provider cost when no provider rule matches: 100 = 1%. */
    defaultMarkupBps: markupBpsSchema,
    /** Basis points added on a top-up payment. */
    topupFeeBps: z.number().int().min(0).max(MAX_TOPUP_FEE_BPS),
    /** The least fee a top-up pays, in micro-SAR (1 SAR = 1,000,000). */
    topupFeeMinMicroSar: z
      .number()
      .int()
      .min(0)
      .max(MAX_TOPUP_FEE_MIN_MICRO_SAR),
    /** The least credit one top-up may buy, in micro-SAR. */
    topupMinMicroSar: topupAmountMicroSarSchema,
    /** The most credit one top-up may buy, in micro-SAR. */
    topupMaxMicroSar: topupAmountMicroSarSchema,
    /** The quick-pick amounts of the top-up form, in micro-SAR. */
    topupPresetsMicroSar: z
      .array(topupAmountMicroSarSchema)
      .min(1)
      .max(MAX_TOPUP_PRESETS),
    /**
     * The official USD to SAR rate × 1,000,000 (3.75 is 3,750,000). The range
     * is a ceiling against a typo, as in the database.
     */
    usdToSarRateE6: z
      .number()
      .int()
      .min(MIN_USD_TO_SAR_RATE_E6, {
        message: "The USD to SAR rate must be between 3 and 5",
      })
      .max(MAX_USD_TO_SAR_RATE_E6, {
        message: "The USD to SAR rate must be between 3 and 5",
      }),
    /** The bank's conversion commission in bps, added to the official rate: 100 = 1%. */
    fxSpreadBps: z.number().int().min(0).max(MAX_FX_SPREAD_BPS, {
      message: "The bank's spread must be between 0% and 10%",
    }),
  })
  .superRefine((value, ctx) => {
    if (value.topupMaxMicroSar < value.topupMinMicroSar) {
      ctx.addIssue({
        code: "custom",
        path: ["topupMaxMicroSar"],
        message: "The top-up maximum must be at least the minimum",
      });
    }
    value.topupPresetsMicroSar.forEach((preset, index) => {
      if (preset < value.topupMinMicroSar || preset > value.topupMaxMicroSar) {
        ctx.addIssue({
          code: "custom",
          path: ["topupPresetsMicroSar", index],
          message: "Each quick-pick amount must be within the top-up limits",
        });
      }
    });
  });

/** The gateway's provider name, as in its config: openai, anthropic, ... */
export const pricingProviderSchema = z
  .string()
  .trim()
  .regex(/^[a-z0-9][a-z0-9_.-]{0,63}$/);

export const setProviderMarkupSchema = z.object({
  markupBps: markupBpsSchema,
});

export type UpdatePricingSettingsInput = z.infer<
  typeof updatePricingSettingsSchema
>;
export type SetProviderMarkupInput = z.infer<typeof setProviderMarkupSchema>;

/** A payment provider the product implements (ADMIN-03). */
export const paymentProviderSchema = z.enum(PAYMENT_PROVIDERS);

export const updatePaymentSettingsSchema = z.object({
  /** Off refuses new checkouts; payments already started still credit. */
  topupsEnabled: z.boolean(),
  /** The provider new checkouts go to. */
  activeProvider: paymentProviderSchema,
});

export const updatePaymentProviderSchema = z.object({
  enabled: z.boolean(),
  mode: z.enum(PAYMENT_MODES),
});

export type UpdatePaymentSettingsInput = z.infer<
  typeof updatePaymentSettingsSchema
>;
export type UpdatePaymentProviderInput = z.infer<
  typeof updatePaymentProviderSchema
>;

/** The gateway's key_id for a provider key. */
export const providerKeyIdSchema = z.string().min(1).max(255);

/**
 * The data terms of one provider key (ADR 0015). Null retention or training
 * is "unknown", which a policy treats as the worst case.
 */
export const setProviderTermsSchema = z.object({
  region: z.enum(PROVIDER_REGIONS),
  retentionDays: z.number().int().min(0).max(36_500).nullable(),
  trainsOnData: z.boolean().nullable(),
  agreement: z.enum(PROVIDER_AGREEMENTS),
  source: z.string().trim().min(1).max(500),
  verifiedOn: z.iso.date(),
});

export type SetProviderTermsInput = z.infer<typeof setProviderTermsSchema>;

/** A provider's name in the gateway: what a model id starts with ("openai/gpt-..."). */
export const providerNameSchema = z
  .string()
  .trim()
  .regex(/^[a-z0-9][a-z0-9_-]{0,62}$/);

/**
 * A provider the gateway knows by name, or one of our own: then `baseProvider`
 * names the protocol it speaks and `baseUrl` where it is.
 */
export const createProviderAccountSchema = z
  .object({
    name: providerNameSchema,
    baseProvider: providerNameSchema.optional(),
    baseUrl: z.url().max(500).optional(),
  })
  .refine((value) => Boolean(value.baseProvider) === Boolean(value.baseUrl), {
    path: ["baseUrl"],
  });

export type CreateProviderAccountInput = z.infer<
  typeof createProviderAccountSchema
>;

const providerKeyModelsSchema = z
  .array(z.string().trim().min(1).max(200))
  .min(1)
  .max(500);

/** `value` is the secret. It is sent to the gateway and stored nowhere else. */
export const createProviderKeySchema = z.object({
  name: z.string().trim().min(1).max(100),
  value: z.string().min(1).max(4000),
  models: providerKeyModelsSchema.optional(),
  weight: z.number().min(0).max(100).optional(),
});

export type CreateProviderKeyInput = z.infer<typeof createProviderKeySchema>;

export const updateProviderKeySchema = z
  .object({
    name: z.string().trim().min(1).max(100).optional(),
    value: z.string().min(1).max(4000).optional(),
    models: providerKeyModelsSchema.optional(),
    weight: z.number().min(0).max(100).optional(),
    isEnabled: z.boolean().optional(),
  })
  .refine((value) => Object.values(value).some((field) => field !== undefined));

export type UpdateProviderKeyInput = z.infer<typeof updateProviderKeySchema>;

/** The staff request log: newest first, optionally one organization or one outcome. */
export const getStaffRequestsInputSchema = z.object({
  organizationId: z.string().min(1).max(100).optional(),
  status: z.enum(["success", "error"]).optional(),
  limit: z.coerce.number().int().min(1).max(100).default(50),
  offset: z.coerce.number().int().min(0).max(100_000).default(0),
});

export type GetStaffRequestsInput = z.infer<typeof getStaffRequestsInputSchema>;

export const getAdminActionsInputSchema = z.object({
  limit: z.coerce.number().int().min(1).max(200).default(100),
  before: z.coerce.number().int().positive().optional(),
  actorId: z.string().min(1).max(100).optional(),
});

export type GetAdminActionsInput = z.infer<typeof getAdminActionsInputSchema>;
