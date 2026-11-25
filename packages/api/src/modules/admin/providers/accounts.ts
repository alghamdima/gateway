import {
  GatewayError,
  GatewayNotConfiguredError,
  getGateway,
} from "@workspace/gateway/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import { removeProviderTerms } from "./providers";

import type {
  CreateProviderAccountInput,
  CreateProviderKeyInput,
  UpdateProviderKeyInput,
} from "../../../schema";
import type { GatewayClient } from "@workspace/gateway/server";

/**
 * Providers and their keys, managed by staff (docs/platform/PLAN.md, "Gateway
 * UI removal"). The gateway owns them: it stores the secret, checks the name
 * and the address, and masks the key when it shows it. This module passes the
 * change on and keeps our data terms in step. No secret is stored or logged
 * on our side.
 */

/** What the gateway said when it refused, for the staff member to act on. */
const refusalReason = (error: GatewayError) => {
  const body = error.message.slice(error.message.indexOf("{"));
  try {
    const parsed: unknown = JSON.parse(body);
    if (
      typeof parsed === "object" &&
      parsed !== null &&
      "error" in parsed &&
      typeof parsed.error === "object" &&
      parsed.error !== null &&
      "message" in parsed.error &&
      typeof parsed.error.message === "string"
    ) {
      return parsed.error.message;
    }
  } catch {
    // not JSON: the status alone decides
  }
  return undefined;
};

const viaGateway = async <T>(run: () => Promise<T>): Promise<T> => {
  try {
    return await run();
  } catch (error) {
    if (error instanceof GatewayNotConfiguredError) {
      throw new HttpException(HttpStatusCode.SERVICE_UNAVAILABLE, {
        code: "error.general",
      });
    }
    if (!(error instanceof GatewayError)) {
      throw error;
    }
    if (error.status === HttpStatusCode.CONFLICT) {
      throw new HttpException(HttpStatusCode.CONFLICT, {
        code: "admin:providers.accounts.error.exists",
      });
    }
    if (error.status === HttpStatusCode.NOT_FOUND) {
      throw new HttpException(HttpStatusCode.NOT_FOUND, {
        code: "error.notFound",
      });
    }
    if (
      error.status === HttpStatusCode.BAD_REQUEST ||
      error.status === HttpStatusCode.FORBIDDEN
    ) {
      // its own checks: a name it does not know, an address it will not call
      throw new HttpException(HttpStatusCode.BAD_REQUEST, {
        code: "admin:providers.accounts.error.refused",
        message: refusalReason(error),
      });
    }
    throw new HttpException(HttpStatusCode.SERVICE_UNAVAILABLE, {
      code: "error.general",
    });
  }
};

export const listProviderAccounts = async (
  gateway: GatewayClient = getGateway(),
) => viaGateway(() => gateway.listProviders());

export const createProviderAccount = async (
  input: CreateProviderAccountInput,
  gateway: GatewayClient = getGateway(),
) => viaGateway(() => gateway.createProvider(input));

/**
 * The gateway deletes the provider's keys with it, so the terms we hold for
 * those keys go too; removing them marks every data policy to be pushed again.
 */
export const deleteProviderAccount = async (
  name: string,
  staffId: string,
  gateway: GatewayClient = getGateway(),
) => {
  const keys = await viaGateway(async () => {
    const provider = (await gateway.listProviders()).find(
      (candidate) => candidate.name === name,
    );
    await gateway.deleteProvider(name);
    return provider?.keys ?? [];
  });
  for (const key of keys) {
    await removeProviderTerms(key.id, staffId);
  }
};

export const createProviderKey = async (
  provider: string,
  input: CreateProviderKeyInput,
  gateway: GatewayClient = getGateway(),
) => viaGateway(() => gateway.createProviderKey(provider, input));

export const updateProviderKey = async (
  provider: string,
  keyId: string,
  input: UpdateProviderKeyInput,
  gateway: GatewayClient = getGateway(),
) => viaGateway(() => gateway.updateProviderKey(provider, keyId, input));

export const deleteProviderKey = async (
  provider: string,
  keyId: string,
  staffId: string,
  gateway: GatewayClient = getGateway(),
) => {
  await viaGateway(() => gateway.deleteProviderKey(provider, keyId));
  await removeProviderTerms(keyId, staffId);
};

export const refreshProviderModels = async (
  provider: string,
  gateway: GatewayClient = getGateway(),
) => viaGateway(() => gateway.refreshProviderModels(provider));

/** The price list the gateway charges from, read again now. */
export const syncProviderPrices = async (
  gateway: GatewayClient = getGateway(),
) => viaGateway(() => gateway.syncPrices());
