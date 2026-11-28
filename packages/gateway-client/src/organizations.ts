import { eq } from "@workspace/db";
import { apiKey, gatewayCustomer } from "@workspace/db/schema";
import { db } from "@workspace/db/server";

import { getGateway } from "./server";

/**
 * Removes an organization's virtual keys and customer from the gateway
 * (docs/platform/adr/0004-key-model.md). Called before the organization is
 * deleted: if the gateway cannot be reached this throws and the deletion
 * stops, so no key outlives its organization. Already-deleted keys and
 * customers count as removed, so a retry is safe.
 */
export const removeOrganizationFromGateway = async (organizationId: string) => {
  const [keys, customers] = await Promise.all([
    db
      .select({ virtualKeyId: apiKey.virtualKeyId })
      .from(apiKey)
      .where(eq(apiKey.organizationId, organizationId)),
    db
      .select({ customerId: gatewayCustomer.customerId })
      .from(gatewayCustomer)
      .where(eq(gatewayCustomer.organizationId, organizationId)),
  ]);
  if (keys.length === 0 && customers.length === 0) {
    return;
  }

  const gateway = getGateway();
  for (const { virtualKeyId } of keys) {
    await gateway.deleteVirtualKey(virtualKeyId);
  }
  for (const { customerId } of customers) {
    await gateway.deleteCustomer(customerId);
  }
};
