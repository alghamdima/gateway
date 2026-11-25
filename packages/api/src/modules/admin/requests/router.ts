import { Hono } from "hono";

import { validate } from "../../../middleware";
import { getStaffRequestsInputSchema } from "../../../schema";
import { getStaffRequests } from "./requests";

import type { User } from "@workspace/auth";

/** Mounted under /api/admin, behind enforceAdmin. */
export const requestsRouter = new Hono<{ Variables: { user: User } }>().get(
  "/",
  validate("query", getStaffRequestsInputSchema),
  async (c) => c.json(await getStaffRequests(c.req.valid("query"))),
);
