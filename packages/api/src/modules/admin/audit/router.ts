import { Hono } from "hono";

import { validate } from "../../../middleware";
import { getAdminActionsInputSchema } from "../../../schema";
import { getAdminActions } from "./queries";

/** GET /api/admin/audit, for staff with audit:read (access.ts). */
export const auditRouter = new Hono().get(
  "/",
  validate("query", getAdminActionsInputSchema),
  async (c) => c.json(await getAdminActions(c.req.valid("query"))),
);
