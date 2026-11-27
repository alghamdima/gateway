import { redirect } from "next/navigation";
import { Suspense } from "react";

import { hasAdminPermission, hasStaffPermission } from "@workspace/auth";
import { Icons } from "@workspace/ui-web/icons";
import { SidebarInset, SidebarProvider } from "@workspace/ui-web/sidebar";

import { pathsConfig } from "~/config/paths";
import { getSession } from "~/lib/auth/server";
import { DashboardActionBar } from "~/modules/common/layout/dashboard/action-bar";
import { DashboardInset } from "~/modules/common/layout/dashboard/inset";
import { DashboardSidebar } from "~/modules/common/layout/dashboard/sidebar/index";
import { DashboardSidebarSkeleton } from "~/modules/common/layout/dashboard/sidebar/skeleton";
import { TurboLink } from "~/modules/common/turbo-link";

import type { StaffPermissions, User } from "@workspace/auth";

const menu = [
  {
    label: "admin",
    items: [
      {
        title: "home",
        href: pathsConfig.admin.index,
        icon: <Icons.Home />,
      },
      {
        title: "users",
        href: pathsConfig.admin.users.index,
        exact: false,
        icon: <Icons.UsersRound />,
      },
      {
        title: "organizations",
        href: pathsConfig.admin.organizations.index,
        exact: false,
        icon: <Icons.Building />,
      },
      {
        title: "customers",
        href: pathsConfig.admin.customers.index,
        exact: false,
        icon: <Icons.HandCoins />,
      },
      {
        title: "pricing",
        href: pathsConfig.admin.pricing.index,
        icon: <Icons.Percent />,
      },
      {
        title: "modelPrices",
        href: pathsConfig.admin.modelPrices.index,
        icon: <Icons.ChartNoAxesGantt />,
      },
      {
        title: "payments",
        href: pathsConfig.admin.payments.index,
        icon: <Icons.CreditCard />,
      },
      {
        title: "promoCodes",
        href: pathsConfig.admin.promoCodes.index,
        exact: false,
        icon: <Icons.Gift />,
      },
      {
        title: "providers",
        href: pathsConfig.admin.providers.index,
        icon: <Icons.Globe2 />,
      },
      {
        title: "requests",
        href: pathsConfig.admin.requests.index,
        icon: <Icons.TrendingUp />,
      },
      {
        title: "audit",
        href: pathsConfig.admin.audit.index,
        icon: <Icons.ScrollText />,
      },
      {
        title: "reconciliation",
        href: pathsConfig.admin.reconciliation.index,
        icon: <Icons.BadgeCheck />,
      },
    ],
  },
];

/** Menu entries only some staff roles may open (ADR 0011). The API refuses the rest anyway. */
const RESTRICTED: Record<string, StaffPermissions> = {
  [pathsConfig.admin.audit.index]: { audit: ["read"] },
  [pathsConfig.admin.providers.index]: { providers: ["read"] },
  [pathsConfig.admin.promoCodes.index]: { promoCodes: ["read"] },
  [pathsConfig.admin.reconciliation.index]: { credits: ["read"] },
};

const menuFor = (user: User) =>
  menu.map((group) => ({
    ...group,
    items: group.items.filter((item) => {
      const needs = RESTRICTED[item.href];
      return !needs || hasStaffPermission(user, needs);
    }),
  }));

export default function AdminLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <SidebarProvider>
      <Suspense fallback={<DashboardSidebarSkeleton />}>
        <AuthenticatedAdminSidebar />
      </Suspense>
      <SidebarInset>
        <DashboardActionBar menu={menu} />
        <DashboardInset>{children}</DashboardInset>
      </SidebarInset>
    </SidebarProvider>
  );
}

async function AuthenticatedAdminSidebar() {
  const session = await getSession();

  if (!session?.user) {
    return redirect(pathsConfig.auth.login);
  }

  const { user } = session;

  if (!hasAdminPermission(user)) {
    return redirect(pathsConfig.dashboard.user.index);
  }

  // staff need two-factor authentication on (ADR 0011); the API refuses them without it
  if (!user.twoFactorEnabled) {
    return redirect(pathsConfig.dashboard.user.settings.security);
  }

  return (
    <DashboardSidebar
      user={user}
      menu={menuFor(user)}
      header={
        <TurboLink
          href={pathsConfig.index}
          className="flex items-center gap-3 p-2 transition-[padding] group-data-[collapsible=icon]:p-0.5"
        >
          <Icons.Logo className="text-primary h-8 transition-[width,height]" />
          <Icons.LogoText className="text-foreground h-4 group-data-[collapsible=icon]:hidden" />
        </TurboLink>
      }
    />
  );
}
