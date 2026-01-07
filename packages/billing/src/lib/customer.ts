import { eq } from "@workspace/db";
import { customer } from "@workspace/db/schema";
import { db } from "@workspace/db/server";

import type { InsertCustomer } from "@workspace/db/schema";

export const getCustomerByUserId = async (userId: string) => {
  const [data] = await db
    .select()
    .from(customer)
    .where(eq(customer.userId, userId));

  return data ?? null;
};

export const getCustomerByCustomerId = async (customerId: string) => {
  const [data] = await db
    .select()
    .from(customer)
    .where(eq(customer.customerId, customerId));

  return data ?? null;
};

export const updateCustomer = (
  userId: string,
  data: Partial<InsertCustomer>,
) => {
  return db.update(customer).set(data).where(eq(customer.userId, userId));
};
export const upsertCustomer = (data: InsertCustomer) => {
  return db.insert(customer).values(data).onConflictDoUpdate({
    target: customer.userId,
    set: data,
  });
};
