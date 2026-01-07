import { eq } from "@workspace/db";
import { customer } from "@workspace/db/schema";
import { db } from "@workspace/db/server";

import type { UpdateCustomer } from "@workspace/db/schema";

export const deleteCustomer = async ({ id }: { id: string }) =>
  db.delete(customer).where(eq(customer.id, id));

export const updateCustomer = async ({
  id,
  data,
}: {
  id: string;
  data: UpdateCustomer;
}) => db.update(customer).set(data).where(eq(customer.id, id));
