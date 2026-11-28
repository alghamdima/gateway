import { Hono } from "hono";

import { validate } from "../../middleware";
import { updateCatalogSettingsSchema } from "../../schema";
import {
  getCatalogSettings,
  getCatalogSettingsChanges,
  updateCatalogSettings,
} from "./settings";

import type { AuditChange } from "../admin/access-middleware";
import type { User } from "@workspace/auth";

/**
 * The catalog settings (CAT-11), mounted under /api/admin/catalog-settings
 * behind enforceAdmin; access rules in admin/access.ts (pricing: read, update,
 * as the other values staff set about what customers are shown). Each change
 * sets auditChange, so the audit log keeps before and after.
 */
export const catalogSettingsAdminRouter = new Hono<{
  Variables: { user: User; auditChange?: AuditChange };
}>()
  .get("/", async (c) => c.json(await getCatalogSettings()))
  .get("/changes", async (c) => c.json(await getCatalogSettingsChanges()))
  .put("/", validate("json", updateCatalogSettingsSchema), async (c) => {
    const { settings, before, after } = await updateCatalogSettings({
      ...c.req.valid("json"),
      staffId: c.var.user.id,
    });
    c.set("auditChange", { before, after });
    return c.json(settings);
  });
