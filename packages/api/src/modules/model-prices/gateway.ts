import {
  GatewayError,
  GatewayNotConfiguredError,
  getGateway,
} from "@workspace/gateway-client/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import type { GatewayClient } from "@workspace/gateway-client/server";

/** A gateway that cannot be reached is a 502; one that is not configured a 503. */
export const withGateway = async <T>(
  run: (gateway: GatewayClient) => Promise<T>,
) => {
  try {
    return await run(getGateway());
  } catch (error) {
    if (error instanceof GatewayNotConfiguredError) {
      throw new HttpException(HttpStatusCode.SERVICE_UNAVAILABLE, {
        code: "error.general",
        message: error.message,
      });
    }
    if (error instanceof GatewayError) {
      throw new HttpException(HttpStatusCode.BAD_GATEWAY, {
        code: "error.general",
        message: error.message,
      });
    }
    throw error;
  }
};
