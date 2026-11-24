import { Hono } from "hono";
import * as z from "zod";

import {
  enforceAuth,
  enforceOrganizationPermission,
  validate,
} from "../../middleware";
import { getActivity } from "./queries";

const activityQuerySchema = z.object({
  days: z.coerce.number().int().min(1).max(365).default(30),
});

/** /api/activity/:organizationId. The same audience as the credits page: billing:read. */
export const activityRouter = new Hono().use(enforceAuth).get(
  "/:organizationId",
  async (c, next) =>
    enforceOrganizationPermission({
      organizationId: c.req.param("organizationId"),
      permissions: { billing: ["read"] },
    })(c, next),
  validate("query", activityQuerySchema),
  async (c) =>
    c.json(
      await getActivity({
        organizationId: c.req.param("organizationId"),
        days: c.req.valid("query").days,
      }),
    ),
);
