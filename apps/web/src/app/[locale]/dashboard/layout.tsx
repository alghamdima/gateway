import { dehydrate, HydrationBoundary } from "@tanstack/react-query";
import { redirect } from "next/navigation";

import { getBillingSummaryResponseSchema } from "@workspace/api/schema";
import { handle } from "@workspace/api/utils";

import { pathsConfig } from "~/config/paths";
import { api } from "~/lib/api/server";
import { getSession } from "~/lib/auth/server";
import { getQueryClient } from "~/lib/query/server";
import { billing } from "~/modules/billing/lib/api";

export default async function DashboardLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  const { user } = await getSession();

  if (!user) {
    return redirect(pathsConfig.auth.login);
  }

  const queryClient = getQueryClient();
  await queryClient.prefetchQuery({
    ...billing.queries.summary.get,
    queryFn: handle(api.billing.me.$get, {
      schema: getBillingSummaryResponseSchema,
    }),
  });

  return (
    <HydrationBoundary state={dehydrate(queryClient)}>
      {children}
    </HydrationBoundary>
  );
}
