import { createMiddleware } from "hono/factory";

import { auth } from "@workspace/auth/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

/** What an impersonated session may still change: ending the impersonation. */
const IMPERSONATION_EXITS = new Set([
  "/api/auth/admin/stop-impersonating",
  "/api/auth/sign-out",
]);

const READ_METHODS = new Set(["GET", "HEAD", "OPTIONS"]);

/**
 * Staff impersonating a customer may look, not act: every request that can
 * change something is refused, in our modules and in Better Auth's own routes
 * (passkeys, invitations, keys, top-ups). Better Auth marks the session with
 * impersonatedBy but has no read-only mode.
 */
export const refuseImpersonatedWrites = createMiddleware(async (c, next) => {
  if (READ_METHODS.has(c.req.method) || IMPERSONATION_EXITS.has(c.req.path)) {
    return next();
  }
  const session = await auth.api.getSession({ headers: c.req.raw.headers });
  if (session?.session.impersonatedBy) {
    throw new HttpException(HttpStatusCode.FORBIDDEN, {
      code: "error.impersonationReadOnly",
    });
  }
  return next();
});
