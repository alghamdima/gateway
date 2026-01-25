import { dehydrate, HydrationBoundary } from "@tanstack/react-query";
import { redirect } from "next/navigation";

import { Icons } from "@workspace/ui-web/icons";
import { SidebarInset, SidebarProvider } from "@workspace/ui-web/sidebar";

import { pathsConfig } from "~/config/paths";
import { getOrganization, getSession } from "~/lib/auth/server";
import { getQueryClient } from "~/lib/query/server";
import { DashboardActionBar } from "~/modules/common/layout/dashboard/action-bar";
import { DashboardInset } from "~/modules/common/layout/dashboard/inset";
import { DashboardSidebar } from "~/modules/common/layout/dashboard/sidebar/index";
import { organization } from "~/modules/organization/lib/api";

const menu = (organization: string) => [
  {
    label: "platform",
    items: [
      {
        title: "home",
        href: pathsConfig.dashboard.organization(organization).index,
        icon: <Icons.Home />,
      },
    ],
  },
  {
    label: "organization",
    items: [
      {
        title: "settings",
        href: pathsConfig.dashboard.organization(organization).settings.index,
        icon: <Icons.Settings />,
        menu: [
          {
            items: [
              {
                title: "general",
                href: pathsConfig.dashboard.organization(organization).settings
                  .index,
                icon: <Icons.Settings />,
              },
            ],
          },
        ],
      },
      {
        title: "members",
        href: pathsConfig.dashboard.organization(organization).members,
        icon: <Icons.UsersRound />,
      },
    ],
  },
];

export default async function DashboardLayout({
  children,
  params,
}: {
  children: React.ReactNode;
  params: Promise<{
    organization: string;
  }>;
}) {
  const { user } = await getSession();

  if (!user) {
    return redirect(pathsConfig.auth.login);
  }

  const organizationSlug = (await params).organization;
  const activeOrganization = await getOrganization({ slug: organizationSlug });

  if (!activeOrganization) {
    return redirect(pathsConfig.dashboard.user.index);
  }

  const queryClient = getQueryClient();
  queryClient.setQueryData(
    organization.queries.get({ slug: organizationSlug }).queryKey,
    activeOrganization,
  );
  queryClient.setQueryData(
    organization.queries.get({ id: activeOrganization.id }).queryKey,
    activeOrganization,
  );

  return (
    <HydrationBoundary state={dehydrate(queryClient)}>
      <SidebarProvider>
        <DashboardSidebar user={user} menu={menu(organizationSlug)} />
        <SidebarInset>
          <DashboardActionBar menu={menu(organizationSlug)} />
          <DashboardInset>{children}</DashboardInset>
        </SidebarInset>
      </SidebarProvider>
    </HydrationBoundary>
  );
}
