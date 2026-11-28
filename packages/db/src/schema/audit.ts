import { sql } from "drizzle-orm";
import {
  bigserial,
  check,
  index,
  jsonb,
  pgTable,
  text,
  timestamp,
} from "drizzle-orm/pg-core";

import { generateId } from "@workspace/shared/utils";

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

/**
 * One daily comparison of what the gateway logged against what the ledger
 * charged (docs/platform/PLAN.md, step 10.1). Findings are kept as written,
 * so a past report can be read as it was.
 */
export const reconciliationRun = pgTable(
  "reconciliation_run",
  {
    id: text().primaryKey().$defaultFn(generateId),
    periodStart: timestamp().notNull(),
    periodEnd: timestamp().notNull(),
    /** ok, mismatch, or error when a source could not be read. */
    status: text().notNull(),
    findings: jsonb().$type<Record<string, unknown>>().notNull(),
    createdAt: timestamp().notNull().defaultNow(),
  },
  (t) => [
    index("reconciliation_run_periodStart_idx").on(t.periodStart),
    check(
      "reconciliation_run_status",
      sql`${t.status} in ('ok', 'mismatch', 'error')`,
    ),
  ],
);
