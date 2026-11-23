import {
  bigserial,
  index,
  jsonb,
  pgTable,
  text,
  timestamp,
} from "drizzle-orm/pg-core";

import { createSelectSchema } from "../lib/zod";

import type * as z from "zod";

/**
 * What staff did in the admin console (docs/platform/adr/0011-staff-roles.md):
 * one row per successful action. Append-only: a trigger rejects UPDATE and
 * DELETE. No foreign keys, so the record outlives the users it names.
 */
export const adminAuditLog = pgTable(
  "admin_audit_log",
  {
    id: bigserial({ mode: "number" }).primaryKey(),
    actorId: text().notNull(),
    /** The actor's roles at the time, as stored on the user. */
    actorRole: text(),
    /** For example "user.ban", "pricing.update", "credits.adjust". */
    action: text().notNull(),
    targetType: text(),
    targetId: text(),
    /** What was changed, without secrets (no passwords, no session tokens). */
    details: jsonb().$type<Record<string, unknown>>(),
    ipAddress: text(),
    createdAt: timestamp().notNull().defaultNow(),
  },
  (t) => [
    index("admin_audit_log_createdAt_idx").on(t.createdAt),
    index("admin_audit_log_actorId_idx").on(t.actorId),
  ],
);

export const selectAdminAuditLogSchema = createSelectSchema(adminAuditLog);
export type SelectAdminAuditLog = z.infer<typeof selectAdminAuditLogSchema>;
