import { Hono } from "hono";
import { createMiddleware } from "hono/factory";

import {
  GatewayError,
  GatewayNotConfiguredError,
} from "@workspace/gateway-client/server";
import { logger } from "@workspace/shared/logger";

import { getGeneration } from "../activity/generations";
import { accessForOrganization } from "../data-policy/policy";
import {
  AUTH_MESSAGES,
  authenticateKey,
  KEY_REMEDY_HINTS,
  readKey,
} from "./auth";
import { listPublicModels } from "./models";
import { describeCredits, describeKey } from "./queries";

import type { CallingKey, KeyRefusal } from "./auth";
import type { ContentfulStatusCode } from "hono/utils/http-status";

/**
 * The OpenRouter-compatible endpoints the control plane answers
 * (docs/platform/PLAN.md, step 8.2). The model endpoints themselves are the
 * gateway's. Errors use the product's error contract, OpenRouter's shape
 * (SPEC API-08, docs/platform/api-errors.md): { error: { code, message,
 * metadata? } }, with metadata on refusals the caller can act on.
 */

interface RefusalMetadata {
  limit_source: string;
  reason: string;
  remedy_hint: string;
}

const apiError = (
  status: ContentfulStatusCode,
  message: string,
  metadata?: RefusalMetadata,
  retryAfterSeconds?: number,
) =>
  Response.json(
    {
      error: metadata
        ? {
            code: status,
            message,
            metadata: retryAfterSeconds
              ? { ...metadata, retry_after_seconds: retryAfterSeconds }
              : metadata,
          }
        : { code: status, message },
    },
    {
      status,
      headers: retryAfterSeconds
        ? { "Retry-After": String(retryAfterSeconds) }
        : undefined,
    },
  );

/** Seconds to wait when the gateway is briefly unreachable. */
const CATALOG_RETRY_AFTER_SECONDS = 5;

/** 401 for a key that is missing, malformed, unknown, disabled or expired. */
const keyRefused = ({ message, reason }: KeyRefusal) =>
  apiError(401, message, {
    limit_source: "key",
    reason,
    remedy_hint: KEY_REMEDY_HINTS[reason],
  });

const enforceKey = createMiddleware<{ Variables: { key: CallingKey } }>(
  async (c, next) => {
    const sent = readKey({
      authorization: c.req.header("authorization"),
      apiKey: c.req.header("x-api-key"),
    });
    if ("message" in sent) {
      return keyRefused(sent);
    }
    const result = await authenticateKey(sent.key);
    if (!result.isAuthenticated) {
      return keyRefused(result);
    }
    c.set("key", result.key);
    return next();
  },
);

export const v1Router = new Hono<{ Variables: { key: CallingKey } }>()
  // public, as OpenRouter's: no key needed to see models and prices. With a
  // key, the list is what that organization's data policy can reach (ADR 0015);
  // a key that is not valid is refused rather than shown the whole catalog.
  .get("/models", async (c) => {
    const sent = readKey({
      authorization: c.req.header("authorization"),
      apiKey: c.req.header("x-api-key"),
    });
    // no credentials at all is the public catalog; anything else must be a valid key
    if ("message" in sent && sent.message !== AUTH_MESSAGES.missing) {
      return keyRefused(sent);
    }
    const caller = "key" in sent ? await authenticateKey(sent.key) : undefined;
    if (caller && !caller.isAuthenticated) {
      return keyRefused(caller);
    }
    try {
      return c.json(
        await listPublicModels(
          undefined,
          undefined,
          caller
            ? await accessForOrganization(caller.key.organizationId)
            : undefined,
        ),
      );
    } catch (error) {
      if (
        error instanceof GatewayError ||
        error instanceof GatewayNotConfiguredError
      ) {
        logger.error(error);
        return apiError(
          503,
          "The model catalog is unavailable. Try again shortly.",
          {
            limit_source: "service",
            reason: "catalog_unavailable",
            remedy_hint:
              "A temporary problem on our side. Retry after the number of seconds in Retry-After.",
          },
          CATALOG_RETRY_AFTER_SECONDS,
        );
      }
      throw error;
    }
  })
  .use("/key", enforceKey)
  .use("/credits", enforceKey)
  .use("/generation", enforceKey)
  .get("/key", async (c) => c.json(await describeKey(c.var.key)))
  .get("/credits", async (c) =>
    c.json(await describeCredits(c.var.key.organizationId)),
  )
  // any key of the organization may read its requests, as OpenRouter's
  .get("/generation", async (c) => {
    const id = c.req.query("id")?.trim();
    if (!id) {
      return apiError(400, "Missing the id query parameter");
    }
    const generation = await getGeneration(c.var.key.organizationId, id);
    return generation
      ? c.json({ data: generation })
      : apiError(404, "Generation not found");
  });
