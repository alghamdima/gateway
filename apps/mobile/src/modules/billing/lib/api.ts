import { mutationOptions, queryOptions } from "@tanstack/react-query";

import { getBillingSummaryResponseSchema } from "@workspace/api/schema";
import { handle } from "@workspace/api/utils";

import { api } from "~/lib/api";

import type { InferRequestType } from "hono/client";

const KEY = "billing";

const queries = {
  summary: {
    get: queryOptions({
      queryKey: [KEY, "me"],
      queryFn: () =>
        handle(api.billing.me.$get, {
          schema: getBillingSummaryResponseSchema,
        })(),
    }),
  },
};

const mutations = {
  portal: mutationOptions({
    mutationKey: [KEY, "portal"],
    mutationFn: (data: InferRequestType<typeof api.billing.portal.$get>) =>
      handle(api.billing.portal.$get)(data),
  }),
};

export const billing = {
  queries,
  mutations,
};
