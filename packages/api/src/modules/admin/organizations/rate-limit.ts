import { eq } from "@workspace/db";
import { organization, organizationRateLimit } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import {
  GatewayError,
  GatewayNotConfiguredError,
  getGateway,
} from "@workspace/gateway/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import { ensureGatewayCustomer } from "../../keys/keys";

import type { GatewayClient } from "@workspace/gateway/server";

/**
 * What staff allow an organization per minute (step G1 in
 * docs/platform/PLAN.md). The gateway enforces it as its customer's rate
 * limit across all the organization's keys; this table is the record.
 */

export interface OrganizationRateLimit {
  requestsPerMinute: number | null;
  tokensPerMinute: number | null;
  updatedBy: string | null;
  updatedAt: Date | null;
}

const NO_LIMIT: OrganizationRateLimit = {
  requestsPerMinute: null,
  tokensPerMinute: null,
  updatedBy: null,
  updatedAt: null,
};

export const getOrganizationRateLimit = async (
  organizationId: string,
): Promise<OrganizationRateLimit> => {
  const [row] = await db
    .select({
      requestsPerMinute: organizationRateLimit.requestsPerMinute,
      tokensPerMinute: organizationRateLimit.tokensPerMinute,
      updatedBy: organizationRateLimit.updatedBy,
      updatedAt: organizationRateLimit.updatedAt,
    })
    .from(organizationRateLimit)
    .where(eq(organizationRateLimit.organizationId, organizationId));
  return row ?? NO_LIMIT;
};

export interface SetOrganizationRateLimitInput {
  organizationId: string;
  staffId: string;
  requestsPerMinute: number | null;
  tokensPerMinute: number | null;
}

/**
 * The gateway first: the limit must take effect even if recording it fails,
 * and a retry sets the same values again. Both null removes the limit.
 */
export const setOrganizationRateLimit = async (
  {
    organizationId,
    staffId,
    requestsPerMinute,
    tokensPerMinute,
  }: SetOrganizationRateLimitInput,
  gateway: GatewayClient = getGateway(),
): Promise<OrganizationRateLimit> => {
  const [org] = await db
    .select({ id: organization.id })
    .from(organization)
    .where(eq(organization.id, organizationId));
  if (!org) {
    throw new HttpException(HttpStatusCode.NOT_FOUND, {
      code: "error.notFound",
    });
  }

  const isRemoval = requestsPerMinute === null && tokensPerMinute === null;
  try {
    const customerId = await ensureGatewayCustomer(organizationId, gateway);
    await gateway.setCustomerRateLimit(
      customerId,
      isRemoval ? null : { requestsPerMinute, tokensPerMinute },
    );
  } catch (error) {
    if (
      error instanceof GatewayError ||
      error instanceof GatewayNotConfiguredError
    ) {
      throw new HttpException(HttpStatusCode.SERVICE_UNAVAILABLE, {
        code: "error.general",
      });
    }
    throw error;
  }

  if (isRemoval) {
    await db
      .delete(organizationRateLimit)
      .where(eq(organizationRateLimit.organizationId, organizationId));
    return NO_LIMIT;
  }

  const values = {
    requestsPerMinute,
    tokensPerMinute,
    updatedBy: staffId,
    updatedAt: new Date(),
  };
  const [row] = await db
    .insert(organizationRateLimit)
    .values({ organizationId, ...values })
    .onConflictDoUpdate({
      target: organizationRateLimit.organizationId,
      set: values,
    })
    .returning({
      requestsPerMinute: organizationRateLimit.requestsPerMinute,
      tokensPerMinute: organizationRateLimit.tokensPerMinute,
      updatedBy: organizationRateLimit.updatedBy,
      updatedAt: organizationRateLimit.updatedAt,
    });
  return row ?? NO_LIMIT;
};
