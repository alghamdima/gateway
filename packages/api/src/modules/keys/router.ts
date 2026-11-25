import { Hono } from "hono";

import {
  GatewayError,
  GatewayNotConfiguredError,
} from "@workspace/gateway-client/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import {
  enforceAuth,
  enforceOrganizationPermission,
  validate,
} from "../../middleware";
import { createApiKeySchema, updateApiKeySchema } from "../../schema";
import { createKey, deleteKey, listKeys, updateKey } from "./keys";

import type { Permissions, User } from "@workspace/auth";

const can = (permission: Permissions["apiKey"]) =>
  createPermissionCheck({ apiKey: permission });

const createPermissionCheck =
  (permissions: Permissions) =>
  async (
    c: Parameters<ReturnType<typeof enforceOrganizationPermission>>[0],
    next: () => Promise<void>,
  ) =>
    enforceOrganizationPermission({
      organizationId: c.req.param("organizationId"),
      permissions,
    })(c, next);

/** A gateway that is down or not configured is the server's problem, not the caller's. */
const gatewayUnavailable = (error: unknown): never => {
  if (
    error instanceof GatewayNotConfiguredError ||
    error instanceof GatewayError
  ) {
    throw new HttpException(HttpStatusCode.SERVICE_UNAVAILABLE, {
      code: "error.general",
    });
  }
  throw error;
};

const notFound = () =>
  new HttpException(HttpStatusCode.NOT_FOUND, { code: "error.notFound" });

/** /api/keys/:organizationId. Owners and admins manage keys; members see them (ADR 0004). */
export const keysRouter = new Hono<{ Variables: { user: User } }>()
  .use(enforceAuth)
  .get("/:organizationId", can(["read"]), async (c) =>
    c.json(await listKeys(c.req.param("organizationId"))),
  )
  .post(
    "/:organizationId",
    can(["create"]),
    validate("json", createApiKeySchema),
    async (c) => {
      const created = await createKey({
        organizationId: c.req.param("organizationId"),
        userId: c.var.user.id,
        ...c.req.valid("json"),
      }).catch(gatewayUnavailable);
      return c.json(created, 201);
    },
  )
  .patch(
    "/:organizationId/:keyId",
    can(["update"]),
    validate("json", updateApiKeySchema),
    async (c) => {
      const updated = await updateKey(
        c.req.param("organizationId"),
        c.req.param("keyId"),
        c.req.valid("json"),
      ).catch(gatewayUnavailable);
      if (!updated) {
        throw notFound();
      }
      return c.json(updated);
    },
  )
  .delete("/:organizationId/:keyId", can(["delete"]), async (c) => {
    const isDeleted = await deleteKey(
      c.req.param("organizationId"),
      c.req.param("keyId"),
    ).catch(gatewayUnavailable);
    if (!isDeleted) {
      throw notFound();
    }
    return c.json({ isDeleted });
  });
