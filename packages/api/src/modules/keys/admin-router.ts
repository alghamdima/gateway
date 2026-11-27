import { Hono } from "hono";

import { validate } from "../../middleware";
import { updateKeySettingsSchema } from "../../schema";
import { applyMaxLifetime } from "./lifetime";
import {
  getKeySettings,
  getKeySettingsChanges,
  updateKeySettings,
} from "./settings";

import type { AuditChange } from "../admin/access-middleware";
import type { User } from "@workspace/auth";

/**
 * The key defaults (ADMIN-12), mounted under /api/admin/key-settings behind
 * enforceAdmin; access rules in admin/access.ts (pricing: read, update, as
 * the other values staff set for customers). Each change sets auditChange, so
 * the audit log keeps before and after, as key_settings_change does.
 */
export const keySettingsAdminRouter = new Hono<{
  Variables: { user: User; auditChange?: AuditChange };
}>()
  .get("/", async (c) => c.json(await getKeySettings()))
  .get("/changes", async (c) => c.json(await getKeySettingsChanges()))
  .put("/", validate("json", updateKeySettingsSchema), async (c) => {
    const { settings, before, after } = await updateKeySettings({
      ...c.req.valid("json"),
      staffId: c.var.user.id,
    });
    // a shorter maximum also cuts the keys that would outlive it, in the
    // gateway and here; what fails is retried by the hourly job
    const lifetime = await applyMaxLifetime({
      maxLifetimeSeconds: settings.maxLifetimeSeconds,
    });
    c.set("auditChange", {
      before,
      after: { ...after, keysShortenedToMaxLifetime: lifetime.shortened },
    });
    return c.json({ settings, lifetime });
  });
