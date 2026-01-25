import { redirect } from "next/navigation";

import { Icons } from "@workspace/ui-web/icons";
import { SidebarInset, SidebarProvider } from "@workspace/ui-web/sidebar";

import { pathsConfig } from "~/config/paths";
import { getSession } from "~/lib/auth/server";
import { DashboardActionBar } from "~/modules/common/layout/dashboard/action-bar";
import { DashboardInset } from "~/modules/common/layout/dashboard/inset";
import { DashboardSidebar } from "~/modules/common/layout/dashboard/sidebar/index";

const menu = [
  {
    label: "platform",
    items: [
      {
        title: "home",
        href: pathsConfig.dashboard.user.index,
        icon: <Icons.Home />,
      },
      {
        title: "ai",
        href: pathsConfig.dashboard.user.ai,
        icon: <Icons.Brain />,
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

export default async function DashboardLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  const { user } = await getSession();

  if (!user) {
    return redirect(pathsConfig.auth.login);
  }

  return (
    <SidebarProvider>
      <DashboardSidebar user={user} menu={menu} />
      <SidebarInset>
        <DashboardActionBar menu={menu} />
        <DashboardInset>{children}</DashboardInset>
      </SidebarInset>
    </SidebarProvider>
  );
}
