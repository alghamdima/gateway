import { Hono } from "hono";

import { validate } from "../../middleware";
import {
  setUserOrganizationLimitSchema,
  updateAccountSettingsSchema,
} from "../../schema";
import {
  getAccountSettings,
  getAccountSettingsChanges,
  getUserOrganizationLimit,
  setUserOrganizationLimit,
  updateAccountSettings,
} from "./settings";

import type { AuditChange } from "../admin/access-middleware";
import type { User } from "@workspace/auth";

/**
 * The account settings (ADR 0020), mounted under /api/admin/account-settings
 * behind enforceAdmin; access rules in admin/access.ts (organizations: read,
 * update). Each change sets auditChange, so the audit log keeps before and
 * after.
 */
export const accountSettingsAdminRouter = new Hono<{
  Variables: { user: User; auditChange?: AuditChange };
}>()
  .get("/", async (c) => c.json(await getAccountSettings()))
  .get("/changes", async (c) => c.json(await getAccountSettingsChanges()))
  .put("/", validate("json", updateAccountSettingsSchema), async (c) => {
    const { settings, before, after } = await updateAccountSettings({
      ...c.req.valid("json"),
      staffId: c.var.user.id,
    });
    c.set("auditChange", { before, after });
    return c.json(settings);
  })
  .get("/users/:userId", async (c) =>
    c.json(await getUserOrganizationLimit(c.req.param("userId"))),
  )
  .put(
    "/users/:userId",
    validate("json", setUserOrganizationLimitSchema),
    async (c) => {
      const { limit, before, after } = await setUserOrganizationLimit({
        ...c.req.valid("json"),
        userId: c.req.param("userId"),
        staffId: c.var.user.id,
      });
      c.set("auditChange", { before, after });
      return c.json(limit);
    },
  );
