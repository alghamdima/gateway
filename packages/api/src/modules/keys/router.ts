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
import {
  createApiKeySchema,
  searchApiKeysSchema,
  updateApiKeySchema,
} from "../../schema";
import { expiresAtFor } from "./expiry";
import {
  createKey,
  deleteKey,
  getKey,
  listKeys,
  searchKeys,
  updateKey,
} from "./keys";
import { getKeySettings } from "./settings";

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

/**
 * /api/keys/:organizationId. Owners and admins manage keys; members see them
 * (ADR 0004). No route returns a key's secret except the create, once.
 */
export const keysRouter = new Hono<{ Variables: { user: User } }>()
  .use(enforceAuth)
  .get("/:organizationId", can(["read"]), async (c) =>
    c.json(await listKeys(c.req.param("organizationId"))),
  )
  // what the create form starts from and allows (ADMIN-12); registered before /:keyId
  .get("/:organizationId/settings", can(["read"]), async (c) => {
    const {
      defaultLimitMicroSar,
      defaultLimitReset,
      defaultExpiry,
      maxLifetime,
      limitPresetsMicroSar,
    } = await getKeySettings();
    return c.json({
      defaultLimitMicroSar,
      defaultLimitReset,
      defaultExpiry,
      maxLifetime,
      limitPresetsMicroSar,
    });
  })
  // by name, prefix or a pasted key, in the body so a key never sits in a URL (KEYS-05)
  .post(
    "/:organizationId/search",
    can(["read"]),
    validate("json", searchApiKeysSchema),
    async (c) =>
      c.json(
        await searchKeys(
          c.req.param("organizationId"),
          c.req.valid("json").query,
        ),
      ),
  )
  .get("/:organizationId/:keyId", can(["read"]), async (c) => {
    const key = await getKey(
      c.req.param("organizationId"),
      c.req.param("keyId"),
    );
    if (!key) {
      throw notFound();
    }
    return c.json(key);
  })
  .post(
    "/:organizationId",
    can(["create"]),
    validate("json", createApiKeySchema),
    async (c) => {
      const { name, expiry, limit } = c.req.valid("json");
      const created = await createKey({
        organizationId: c.req.param("organizationId"),
        userId: c.var.user.id,
        name,
        limit,
        expiresAt: expiresAtFor(expiry),
      }).catch(gatewayUnavailable);
      return c.json(created, 201);
    },
  )
  .patch(
    "/:organizationId/:keyId",
    can(["update"]),
    validate("json", updateApiKeySchema),
    async (c) => {
      // the expiry is not among them: the schema refuses any attempt to change it (KEYS-03)
      const { name, disabled, limit } = c.req.valid("json");
      const updated = await updateKey(
        c.req.param("organizationId"),
        c.req.param("keyId"),
        { name, disabled, limit },
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
