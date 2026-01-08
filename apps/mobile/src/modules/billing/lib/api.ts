import { queryOptions } from "@tanstack/react-query";

import { handle } from "@workspace/api/utils";

import { api } from "~/lib/api";

const KEY = "billing";

const queries = {
  customer: {
    get: queryOptions({
      queryKey: [KEY, "customer"],
      queryFn: () => handle(api.billing.customer.$get)(),
    }),
  },
};

export const billing = {
  queries,
} as const;
