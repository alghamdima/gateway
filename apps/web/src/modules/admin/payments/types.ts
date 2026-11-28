import type { InferResponseType } from "hono/client";
import type { api } from "~/lib/api/client";

/** The admin payments overview as the API sends it (packages/api/src/modules/payments). */
export type PaymentsOverview = InferResponseType<
  typeof api.admin.payments.$get,
  200
>;

export type PaymentProviderView = PaymentsOverview["providers"][number];

export type PaymentChange = InferResponseType<
  typeof api.admin.payments.changes.$get,
  200
>[number];
