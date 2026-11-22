import { Hono } from "hono";
import * as z from "zod";

import {
  enforceAuth,
  enforceOrganizationPermission,
  validate,
} from "../../middleware";
import { getBalance, getEntries } from "./ledger";

const getEntriesQuerySchema = z.object({
  limit: z.coerce.number().int().min(1).max(100).default(25),
  before: z.string().optional(),
});

export const creditsRouter = new Hono()
  .use(enforceAuth)
  .get(
    "/:organizationId",
    async (c, next) =>
      enforceOrganizationPermission({
        organizationId: c.req.param("organizationId"),
        permissions: { billing: ["read"] },
      })(c, next),
    async (c) => c.json(await getBalance(c.req.param("organizationId"))),
  )
  .get(
    "/:organizationId/entries",
    async (c, next) =>
      enforceOrganizationPermission({
        organizationId: c.req.param("organizationId"),
        permissions: { billing: ["read"] },
      })(c, next),
    validate("query", getEntriesQuerySchema),
    async (c) =>
      c.json(
        await getEntries({
          organizationId: c.req.param("organizationId"),
          ...c.req.valid("query"),
        }),
      ),
  );
