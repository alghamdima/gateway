import { dehydrate, HydrationBoundary } from "@tanstack/react-query";

import { getBillingSummaryResponseSchema } from "@workspace/api/schema";
import { handle } from "@workspace/api/utils";

import { api } from "~/lib/api/server";
import { getSession } from "~/lib/auth/server";
import { getQueryClient } from "~/lib/query/server";
import { billing } from "~/modules/billing/lib/api";

import { PricingSection } from "./section";

export const Pricing = async () => {
  const { user } = await getSession();

  const queryClient = getQueryClient();

  if (user) {
    await queryClient.prefetchQuery({
      ...billing.queries.summary.get,
      queryFn: handle(api.billing.me.$get, {
        schema: getBillingSummaryResponseSchema,
      }),
    });
  }

  return (
    <HydrationBoundary state={dehydrate(queryClient)}>
      <PricingSection user={user} />
    </HydrationBoundary>
  );
};
