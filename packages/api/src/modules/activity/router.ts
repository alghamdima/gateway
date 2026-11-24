import { Hono } from "hono";
import * as z from "zod";

import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import {
  enforceAuth,
  enforceOrganizationPermission,
  validate,
} from "../../middleware";
import { getGeneration, listRecentRequests } from "./generations";
import { getActivity } from "./queries";

const activityQuerySchema = z.object({
  days: z.coerce.number().int().min(1).max(365).default(30),
});

const recentQuerySchema = z.object({
  limit: z.coerce.number().int().min(1).max(100).default(25),
  before: z.iso.datetime().optional(),
});

const enforceBillingRead = async (
  c: Parameters<ReturnType<typeof enforceOrganizationPermission>>[0],
  next: () => Promise<void>,
) =>
  enforceOrganizationPermission({
    organizationId: c.req.param("organizationId") ?? "",
    permissions: { billing: ["read"] },
  })(c, next);

/** /api/activity/:organizationId. The same audience as the credits page: billing:read. */
export const activityRouter = new Hono()
  .use(enforceAuth)
  .get(
    "/:organizationId",
    enforceBillingRead,
    validate("query", activityQuerySchema),
    async (c) =>
      c.json(
        await getActivity({
          organizationId: c.req.param("organizationId"),
          days: c.req.valid("query").days,
        }),
      ),
  )
  .get(
    "/:organizationId/requests",
    enforceBillingRead,
    validate("query", recentQuerySchema),
    async (c) => {
      const { limit, before } = c.req.valid("query");
      return c.json(
        await listRecentRequests({
          organizationId: c.req.param("organizationId"),
          limit,
          before: before ? new Date(before) : undefined,
        }),
      );
    },
  )
  .get("/:organizationId/requests/:id", enforceBillingRead, async (c) => {
    const generation = await getGeneration(
      c.req.param("organizationId"),
      c.req.param("id"),
    );
    if (!generation) {
      throw new HttpException(HttpStatusCode.NOT_FOUND, {
        code: "error.notFound",
      });
    }
    return c.json(generation);
  });
