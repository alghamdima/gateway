import { Hono } from "hono";
import * as z from "zod";

import { validate } from "../../middleware";
import {
  listContactRequestsSchema,
  updateContactRequestSchema,
  updateSiteSettingsSchema,
} from "../../schema";
import {
  getContactRequest,
  listContactRequests,
  updateContactRequest,
} from "./contact";
import {
  getSiteSettings,
  getSiteSettingsChanges,
  updateSiteSettings,
} from "./settings";

import type { AuditChange } from "../admin/access-middleware";
import type { User } from "@workspace/auth";

const idParam = z.object({ id: z.coerce.number().int().positive() });

/**
 * The site settings (SITE-01), mounted under /api/admin/site-settings behind
 * enforceAdmin; access rules in admin/access.ts (site: read, update). Each
 * change sets auditChange, so the audit log keeps before and after.
 */
export const siteSettingsAdminRouter = new Hono<{
  Variables: { user: User; auditChange?: AuditChange };
}>()
  .get("/", async (c) => c.json(await getSiteSettings()))
  .get("/changes", async (c) => c.json(await getSiteSettingsChanges()))
  .put("/", validate("json", updateSiteSettingsSchema), async (c) => {
    const { settings, before, after } = await updateSiteSettings({
      ...c.req.valid("json"),
      staffId: c.var.user.id,
    });
    c.set("auditChange", { before, after });
    return c.json(settings);
  });

/**
 * The contact messages (SITE-03), mounted under /api/admin/contact-requests;
 * access rules in admin/access.ts (contact: read, update).
 */
export const contactRequestsAdminRouter = new Hono<{
  Variables: { user: User; auditChange?: AuditChange };
}>()
  .get("/", validate("query", listContactRequestsSchema), async (c) =>
    c.json(await listContactRequests(c.req.valid("query"))),
  )
  .get("/:id", validate("param", idParam), async (c) =>
    c.json(await getContactRequest(c.req.valid("param").id)),
  )
  .put(
    "/:id",
    validate("param", idParam),
    validate("json", updateContactRequestSchema),
    async (c) => {
      const { id } = c.req.valid("param");
      const { before, after } = await updateContactRequest({
        ...c.req.valid("json"),
        id,
        staffId: c.var.user.id,
      });
      c.set("auditChange", { before, after });
      return c.json(await getContactRequest(id));
    },
  );
