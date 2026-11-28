import { createGatewayClient } from "./client";
import { env } from "./env";

import type { GatewayClient } from "./client";

export * from "./client";
export * from "./organizations";

/** The gateway is not configured (GATEWAY_URL is unset). */
export class GatewayNotConfiguredError extends Error {
  constructor() {
    super(
      "GATEWAY_URL is not set: the gateway's management API cannot be reached",
    );
    this.name = "GatewayNotConfiguredError";
  }
}

let client: GatewayClient | undefined;

/** The client for the gateway in GATEWAY_URL. Throws when it is not configured. */
export const getGateway = (): GatewayClient => {
  if (!env.GATEWAY_URL) {
    throw new GatewayNotConfiguredError();
  }
  client ??= createGatewayClient({
    url: env.GATEWAY_URL,
    username: env.GATEWAY_ADMIN_USERNAME,
    password: env.GATEWAY_ADMIN_PASSWORD,
  });
  return client;
};
