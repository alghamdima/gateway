import { eq, inArray } from "@workspace/db";
import { gatewayCustomer, organization } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import {
  GatewayError,
  GatewayNotConfiguredError,
  getGateway,
} from "@workspace/gateway/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import type { GetStaffRequestsInput } from "../../../schema";
import type { GatewayClient, GatewayRequest } from "@workspace/gateway/server";

/**
 * The request log for staff (docs/platform/PLAN.md, "Gateway UI removal"):
 * every request the gateway logged, failed ones included, so support can see
 * what happened to a customer's call. The gateway keeps the log and filters
 * it; this module names the organization behind each row. It shows what
 * happened and never what was said: the prompt and the answer stay in the
 * gateway.
 */

export interface StaffRequest extends GatewayRequest {
  /** Null for a request the gateway could not attribute, or whose organization is gone. */
  organization: { id: string; name: string; slug: string } | null;
}

export const getStaffRequests = async (
  { organizationId, status, limit, offset }: GetStaffRequestsInput,
  gateway: GatewayClient = getGateway(),
): Promise<{ requests: StaffRequest[]; total: number }> => {
  let customerIds: string[] | undefined;
  if (organizationId) {
    const customers = await db
      .select({ customerId: gatewayCustomer.customerId })
      .from(gatewayCustomer)
      .where(eq(gatewayCustomer.organizationId, organizationId));
    if (customers.length === 0) {
      // no gateway customer: it never made a request, and asking the gateway
      // with no customer filter would list everybody's
      return { requests: [], total: 0 };
    }
    customerIds = customers.map((customer) => customer.customerId);
  }

  let page: Awaited<ReturnType<GatewayClient["searchRequests"]>>;
  try {
    page = await gateway.searchRequests({ customerIds, status, limit, offset });
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

  const ids = [
    ...new Set(
      page.requests.flatMap((request) =>
        request.customerId ? [request.customerId] : [],
      ),
    ),
  ];
  const owners =
    ids.length === 0
      ? []
      : await db
          .select({
            customerId: gatewayCustomer.customerId,
            id: organization.id,
            name: organization.name,
            slug: organization.slug,
          })
          .from(gatewayCustomer)
          .innerJoin(
            organization,
            eq(organization.id, gatewayCustomer.organizationId),
          )
          .where(inArray(gatewayCustomer.customerId, ids));
  const ownerOf = new Map(
    owners.map(({ customerId, ...owner }) => [customerId, owner]),
  );

  return {
    requests: page.requests.map((request) => ({
      ...request,
      organization:
        (request.customerId && ownerOf.get(request.customerId)) || null,
    })),
    total: page.total,
  };
};
