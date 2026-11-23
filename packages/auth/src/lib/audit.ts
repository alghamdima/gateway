import { adminAuditLog } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { logger } from "@workspace/shared/logger";

/** One successful staff action (docs/platform/adr/0011-staff-roles.md). */
export interface StaffAction {
  actorId: string;
  actorRole: string | null;
  /** For example "user.ban", "pricing.update", "credits.adjust". */
  action: string;
  targetType?: string;
  targetId?: string;
  /** Never passwords or session tokens. */
  details?: Record<string, unknown>;
  ipAddress?: string | null;
}

/**
 * Appends to admin_audit_log. Called after the action succeeded, so a failure
 * here cannot undo it: it is logged as an error for monitoring instead of
 * being thrown at the staff member whose action already took effect.
 */
export const recordStaffAction = async (entry: StaffAction) => {
  try {
    await db.insert(adminAuditLog).values(entry);
  } catch (error) {
    logger.error("admin audit log write failed", {
      action: entry.action,
      actorId: entry.actorId,
      targetId: entry.targetId,
      error,
    });
  }
};

/** The client address as the proxy in front of the app reports it. */
export const clientIp = (headers: Headers | undefined) =>
  headers?.get("x-forwarded-for")?.split(",")[0]?.trim() ??
  headers?.get("x-real-ip") ??
  null;

interface AuthAdminAction {
  action: string;
  /** Only the fields worth keeping; the request body may hold passwords. */
  details?: (body: Record<string, unknown>) => Record<string, unknown>;
}

/** Better Auth admin endpoints that change something. Reads are not recorded. */
export const AUTH_ADMIN_ACTIONS: Record<string, AuthAdminAction> = {
  "/admin/create-user": {
    action: "user.create",
    details: (body) => ({ email: body.email, role: body.role }),
  },
  "/admin/set-role": {
    action: "user.set-role",
    details: (body) => ({ role: body.role }),
  },
  "/admin/ban-user": {
    action: "user.ban",
    details: (body) => ({
      banReason: body.banReason,
      banExpiresIn: body.banExpiresIn,
    }),
  },
  "/admin/unban-user": { action: "user.unban" },
  "/admin/impersonate-user": { action: "user.impersonate" },
  "/admin/stop-impersonating": { action: "user.stop-impersonating" },
  "/admin/revoke-user-session": { action: "session.revoke" },
  "/admin/revoke-user-sessions": { action: "session.revoke-all" },
  "/admin/remove-user": { action: "user.delete" },
  "/admin/set-user-password": { action: "user.set-password" },
  "/admin/update-user": {
    action: "user.update",
    details: (body) => ({
      fields:
        body.data && typeof body.data === "object"
          ? Object.keys(body.data)
          : [],
    }),
  },
};
