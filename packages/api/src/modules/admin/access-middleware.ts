import { createMiddleware } from "hono/factory";

import { clientIp, recordStaffAction } from "@workspace/auth/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import {
  auditTarget,
  canAccessAdminRoute,
  findAccessRule,
  withoutSecrets,
} from "./access";

import type { User } from "@workspace/auth";

/**
 * What a change replaced, when the route knows it: a route sets the
 * `auditChange` variable and the audit entry keeps it beside the request body.
 */
export interface AuditChange {
  before: unknown;
  after: unknown;
}

/**
 * Mounted on /api/admin after enforceAdmin, which has already required a
 * staff role. Refuses what the user's roles do not allow, and records every
 * successful change in the audit log (ADR 0011).
 */
export const enforceStaffAccess = createMiddleware<{
  Variables: { user: User; auditChange?: AuditChange };
}>(async (c, next) => {
  const path = c.req.path.replace(/^.*?\/api\/admin/, "") || "/";
  const user = c.var.user;
  if (!canAccessAdminRoute(user, c.req.method, path)) {
    throw new HttpException(HttpStatusCode.FORBIDDEN, {
      code: "error.forbidden",
    });
  }

  const audit = findAccessRule(c.req.method, path)?.audit;
  // read before the route consumes the body; a rule names the fields that are secrets
  const body: unknown =
    audit && c.req.header("content-type")?.includes("application/json")
      ? await c.req.raw
          .clone()
          .json()
          .catch(() => undefined)
      : undefined;

  await next();

  if (audit && c.res.status < 400) {
    await recordStaffAction({
      actorId: user.id,
      actorRole: user.role ?? null,
      action: audit.action,
      targetType: audit.target,
      targetId: auditTarget(path),
      details: {
        method: c.req.method,
        path,
        body: withoutSecrets(body, audit.secrets ?? []),
        ...c.var.auditChange,
      },
      ipAddress: clientIp(c.req.raw.headers),
    });
  }
});
