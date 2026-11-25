import { dehydrate, HydrationBoundary } from "@tanstack/react-query";
import { redirect } from "next/navigation";
import { Suspense } from "react";

import { getBillingSummaryResponseSchema } from "@workspace/api/schema";
import { handle } from "@workspace/api/utils";
import { Icons } from "@workspace/ui-web/icons";
import { SidebarInset, SidebarProvider } from "@workspace/ui-web/sidebar";

import { pathsConfig } from "~/config/paths";
import { api } from "~/lib/api/server";
import { getSession } from "~/lib/auth/server";
import { getQueryClient } from "~/lib/query/server";
import { billing } from "~/modules/billing/lib/api";
import { DashboardActionBar } from "~/modules/common/layout/dashboard/action-bar";
import { DashboardInset } from "~/modules/common/layout/dashboard/inset";
import { DashboardSidebar } from "~/modules/common/layout/dashboard/sidebar/index";
import { DashboardSidebarSkeleton } from "~/modules/common/layout/dashboard/sidebar/skeleton";

const menu = [
  {
    label: "platform",
    items: [
      {
        title: "home",
        href: pathsConfig.dashboard.user.index,
        icon: <Icons.Home />,
      },
    ],
  },
  {
    label: "account",
    items: [
      {
        title: "settings",
        href: pathsConfig.dashboard.user.settings.index,
        icon: <Icons.Settings />,
        menu: [
          {
            items: [
              {
                title: "general",
                href: pathsConfig.dashboard.user.settings.index,
                icon: <Icons.Settings />,
              },
              {
                title: "security",
                href: pathsConfig.dashboard.user.settings.security,
                icon: <Icons.Lock />,
              },
              {
                title: "billing",
                href: pathsConfig.dashboard.user.settings.billing,
                icon: <Icons.CreditCard />,
              },
            ],
          },
        ],
      },
    ],
  },
];

export default function DashboardLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <SidebarProvider>
      <Suspense fallback={<DashboardSidebarSkeleton />}>
        <AuthenticatedSidebar />
      </Suspense>
      <SidebarInset>
        <DashboardActionBar menu={menu} />
        <DashboardInset>
          <SidebarQueries>{children}</SidebarQueries>
        </DashboardInset>
      </SidebarInset>
    </SidebarProvider>
  );
}

async function AuthenticatedSidebar() {
  const session = await getSession();

  if (!session?.user) {
    return redirect(pathsConfig.auth.login);
  }

  const { user } = session;

  return (
    <HydrationBoundary state={await sidebarQueries(user.id)}>
      <DashboardSidebar user={user} menu={menu} />
    </HydrationBoundary>
  );
}

/**
 * The page gets what the sidebar reads as well. A page that reads the same query can render
 * first, and a query that already exists is only filled in by an effect: the server then sent
 * the sidebar without its data, and the browser, which had fetched it by then, disagreed.
 */
async function SidebarQueries({ children }: { children: React.ReactNode }) {
  const session = await getSession();

  if (!session?.user) {
    return children;
  }

  return (
    <HydrationBoundary state={await sidebarQueries(session.user.id)}>
      {children}
    </HydrationBoundary>
  );
}

async function sidebarQueries(userId: string) {
  const queryClient = getQueryClient();
  await queryClient.query({
    ...billing.queries.summary.get(userId),
    queryFn: () =>
      handle(api.billing.summary.$get, {
        schema: getBillingSummaryResponseSchema,
      })({
        query: {
          referenceId: userId,
        },
      }),
  });

  return dehydrate(queryClient);
}
