import { Hono } from "hono";

import {
  enforceAuth,
  enforceOrganizationPermission,
  validate,
} from "../../middleware";
import { setDataPolicySchema } from "../../schema";
import { getDataPolicy, setDataPolicy } from "./policy";

import type { Permissions, User } from "@workspace/auth";

const can =
  (permission: Permissions["apiKey"]) =>
  async (
    c: Parameters<ReturnType<typeof enforceOrganizationPermission>>[0],
    next: () => Promise<void>,
  ) =>
    enforceOrganizationPermission({
      organizationId: c.req.param("organizationId"),
      permissions: { apiKey: permission },
    })(c, next);

/**
 * /api/data-policy/:organizationId. The policy decides what the
 * organization's keys may reach, so it follows the keys' permissions: members
 * see it, owners and admins change it (ADR 0004, ADR 0015).
 */
export const dataPolicyRouter = new Hono<{ Variables: { user: User } }>()
  .use(enforceAuth)
  .get("/:organizationId", can(["read"]), async (c) =>
    c.json(await getDataPolicy(c.req.param("organizationId"))),
  )
  .put(
    "/:organizationId",
    can(["update"]),
    validate("json", setDataPolicySchema),
    async (c) =>
      c.json(
        await setDataPolicy({
          organizationId: c.req.param("organizationId"),
          userId: c.var.user.id,
          ...c.req.valid("json"),
        }),
      ),
  );
