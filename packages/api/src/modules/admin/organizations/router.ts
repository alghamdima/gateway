import { Hono } from "hono";

import { updateMemberSchema, updateOrganizationSchema } from "@workspace/auth";

import { validate } from "../../../middleware";
import {
  adjustOrganizationCreditsSchema,
  getInvitationsInputSchema,
  getMembersInputSchema,
  getOrganizationOrdersInputSchema,
  getOrganizationSubscriptionsInputSchema,
  getOrganizationsInputSchema,
  setOrganizationRateLimitSchema,
} from "../../../schema";
import { adjustCredits } from "../../credits/adjust";
import { getBalance } from "../../credits/ledger";
import { getDataPolicy } from "../../data-policy/policy";
import { getInvitations } from "../../organization/queries/invitations";
import { getMembers } from "../../organization/queries/members";
import {
  deleteOrganization,
  deleteOrganizationInvitation,
  deleteOrganizationMember,
  updateOrganizationMember,
} from "./mutations";
import { updateOrganization } from "./mutations";
import {
  getOrganizationOrders,
  getOrganizations,
  getOrganization,
  getOrganizationSubscriptions,
} from "./queries";
import {
  getOrganizationRateLimit,
  setOrganizationRateLimit,
} from "./rate-limit";

import type { User } from "@workspace/auth";

export const organizationsRouter = new Hono<{ Variables: { user: User } }>()
  .get("/", validate("query", getOrganizationsInputSchema), async (c) =>
    c.json(await getOrganizations(c.req.valid("query"))),
  )
  .get("/:id", async (c) =>
    c.json(await getOrganization({ id: c.req.param("id") })),
  )
  .get(
    "/:id/subscriptions",
    validate("query", getOrganizationSubscriptionsInputSchema),
    async (c) =>
      c.json(
        await getOrganizationSubscriptions({
          organizationId: c.req.param("id"),
          ...c.req.valid("query"),
        }),
      ),
  )
  .get(
    "/:id/orders",
    validate("query", getOrganizationOrdersInputSchema),
    async (c) =>
      c.json(
        await getOrganizationOrders({
          organizationId: c.req.param("id"),
          ...c.req.valid("query"),
        }),
      ),
  )
  .get("/:id/credits", async (c) => c.json(await getBalance(c.req.param("id"))))
  .post(
    "/:id/credits",
    validate("json", adjustOrganizationCreditsSchema),
    async (c) =>
      c.json(
        await adjustCredits({
          organizationId: c.req.param("id"),
          staffId: c.var.user.id,
          ...c.req.valid("json"),
        }),
      ),
  )
  .get("/:id/rate-limit", async (c) =>
    c.json(await getOrganizationRateLimit(c.req.param("id"))),
  )
  // read-only here: the organization's owners and admins set it (ADR 0015)
  .get("/:id/data-policy", async (c) =>
    c.json(await getDataPolicy(c.req.param("id"))),
  )
  .put(
    "/:id/rate-limit",
    validate("json", setOrganizationRateLimitSchema),
    async (c) =>
      c.json(
        await setOrganizationRateLimit({
          organizationId: c.req.param("id"),
          staffId: c.var.user.id,
          ...c.req.valid("json"),
        }),
      ),
  )
  .delete("/:id", async (c) =>
    c.json(await deleteOrganization({ id: c.req.param("id") })),
  )
  .patch("/:id", validate("json", updateOrganizationSchema), async (c) =>
    c.json(
      await updateOrganization({
        id: c.req.param("id"),
        data: c.req.valid("json"),
      }),
    ),
  )
  .get("/:id/members", validate("query", getMembersInputSchema), async (c) =>
    c.json(
      await getMembers({
        organizationId: c.req.param("id"),
        ...c.req.valid("query"),
      }),
    ),
  )
  .patch(
    "/:id/members/:memberId",
    validate("json", updateMemberSchema),
    async (c) =>
      c.json(
        await updateOrganizationMember({
          id: c.req.param("memberId"),
          organizationId: c.req.param("id"),
          data: c.req.valid("json"),
        }),
      ),
  )
  .delete("/:id/members/:memberId", async (c) =>
    c.json(
      await deleteOrganizationMember({
        id: c.req.param("memberId"),
        organizationId: c.req.param("id"),
      }),
    ),
  )
  .get(
    "/:id/invitations",
    validate("query", getInvitationsInputSchema),
    async (c) =>
      c.json(
        await getInvitations({
          organizationId: c.req.param("id"),
          ...c.req.valid("query"),
        }),
      ),
  )
  .delete("/:id/invitations/:invitationId", async (c) =>
    c.json(
      await deleteOrganizationInvitation({
        id: c.req.param("invitationId"),
        organizationId: c.req.param("id"),
      }),
    ),
  );
