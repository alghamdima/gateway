import { dehydrate, HydrationBoundary } from "@tanstack/react-query";
import { redirect } from "next/navigation";
import { Suspense } from "react";

import { getBillingSummaryResponseSchema } from "@workspace/api/schema";
import { handle } from "@workspace/api/utils";
import { Member, toMemberRole } from "@workspace/auth";
import { Icons } from "@workspace/ui-web/icons";
import { SidebarInset, SidebarProvider } from "@workspace/ui-web/sidebar";

import { pathsConfig } from "~/config/paths";
import { api } from "~/lib/api/server";
import { authClient } from "~/lib/auth/client";
import {
  getMemberByUserId,
  getOrganization,
  getPersonalAccountForSession,
  getSession,
} from "~/lib/auth/server";
import { getQueryClient } from "~/lib/query/server";
import { billing } from "~/modules/billing/lib/api";
import { DashboardActionBar } from "~/modules/common/layout/dashboard/action-bar";
import { DashboardInset } from "~/modules/common/layout/dashboard/inset";
import { DashboardSidebar } from "~/modules/common/layout/dashboard/sidebar/index";
import { DashboardSidebarSkeleton } from "~/modules/common/layout/dashboard/sidebar/skeleton";
import { organization } from "~/modules/organization/lib/api";

import type { Menu } from "~/modules/common/layout/dashboard/sidebar/types";

const menu = ({
  slug,
  member,
  isPersonal,
}: {
  slug: string;
  member: Member | null;
  /** The user's personal account (ADR 0020): no members, no organization settings. */
  isPersonal: boolean;
}): Menu => [
  {
    label: "platform",
    items: [
      {
        title: "home",
        href: pathsConfig.dashboard.organization(slug).index,
        icon: <Icons.Home />,
      },
      {
        title: "apiKeys",
        href: pathsConfig.dashboard.organization(slug).keys,
        // a key's own page is under it
        exact: false,
        icon: <Icons.Key />,
      },
      {
        title: "dataPolicy",
        href: pathsConfig.dashboard.organization(slug).dataPolicy,
        icon: <Icons.Globe2 />,
      },
      ...(authClient.organization.checkRolePermission({
        permissions: { billing: ["read"] },
        role: toMemberRole(member?.role),
      })
        ? [
            {
              title: "activity",
              href: pathsConfig.dashboard.organization(slug).activity,
              icon: <Icons.TrendingUp />,
            },
            {
              title: "credits",
              href: pathsConfig.dashboard.organization(slug).credits,
              icon: <Icons.HandCoins />,
            },
          ]
        : []),
    ],
  },
  isPersonal ? accountGroup : organizationGroup({ slug, member }),
];

// in a personal account the settings are the user's own
const accountGroup: Menu[number] = {
  label: "account",
  items: [
    {
      title: "settings",
      href: pathsConfig.dashboard.user.settings.index,
      icon: <Icons.Settings />,
    },
  ],
};

const organizationGroup = ({
  slug,
  member,
}: {
  slug: string;
  member: Member | null;
}): Menu[number] => ({
  label: "organization",
  items: [
    {
      title: "settings",
      href: pathsConfig.dashboard.organization(slug).settings.index,
      icon: <Icons.Settings />,
      menu: [
        {
          items: [
            {
              title: "general",
              href: pathsConfig.dashboard.organization(slug).settings.index,
              icon: <Icons.Settings />,
            },
            ...(authClient.organization.checkRolePermission({
              permissions: {
                billing: ["read"],
              },
              role: toMemberRole(member?.role),
            })
              ? [
                  {
                    title: "billing",
                    href: pathsConfig.dashboard.organization(slug).settings
                      .billing,
                    icon: <Icons.CreditCard />,
                  },
                ]
              : []),
          ],
        },
      ],
    },
    {
      title: "members",
      href: pathsConfig.dashboard.organization(slug).members,
      icon: <Icons.UsersRound />,
    },
  ],
});

export default function DashboardLayout({
  children,
  params,
}: {
  children: React.ReactNode;
  params: Promise<{
    organization: string;
  }>;
}) {
  return (
    <SidebarProvider>
      <Suspense fallback={<DashboardSidebarSkeleton />}>
        <AuthenticatedOrgSidebar params={params} />
      </Suspense>
      <SidebarInset>
        <Suspense fallback={<div className="h-14 border-b" />}>
          <AuthenticatedOrgActionBar params={params} />
        </Suspense>
        <DashboardInset>
          <SidebarQueries params={params}>{children}</SidebarQueries>
        </DashboardInset>
      </SidebarInset>
    </SidebarProvider>
  );
}

async function getWorkspace(params: Promise<{ organization: string }>) {
  const session = await getSession();

  if (!session?.user) {
    return redirect(pathsConfig.auth.login);
  }

  const { user } = session;

  const organizationSlug = (await params).organization;
  const activeOrganization = await getOrganization({ slug: organizationSlug });

  if (!activeOrganization) {
    return redirect(pathsConfig.dashboard.user.index);
  }

  const personal = await getPersonalAccountForSession();
  // a new user is welcomed first (ONB-03), whichever account they open
  if (personal && !personal.onboardedAt) {
    return redirect(pathsConfig.onboarding);
  }
  const items = menu({
    slug: organizationSlug,
    isPersonal: personal?.id === activeOrganization.id,
    member: await getMemberByUserId({
      organizationId: activeOrganization.id,
      userId: user.id,
    }),
  });

  return { user, activeOrganization, organizationSlug, items };
}

async function AuthenticatedOrgSidebar({
  params,
}: {
  params: Promise<{ organization: string }>;
}) {
  const { user, activeOrganization, organizationSlug, items } =
    await getWorkspace(params);

  return (
    <HydrationBoundary
      state={await sidebarQueries(organizationSlug, activeOrganization)}
    >
      <DashboardSidebar user={user} menu={items} />
    </HydrationBoundary>
  );
}

/**
 * The page gets what the sidebar reads as well. A page that reads the same query can render
 * first, and a query that already exists is only filled in by an effect: the server then sent
 * the sidebar without its data, and the browser, which had fetched it by then, disagreed.
 */
async function SidebarQueries({
  children,
  params,
}: {
  children: React.ReactNode;
  params: Promise<{ organization: string }>;
}) {
  const slug = (await params).organization;
  const activeOrganization = await getOrganization({ slug });

  if (!activeOrganization) {
    return children;
  }

  return (
    <HydrationBoundary state={await sidebarQueries(slug, activeOrganization)}>
      {children}
    </HydrationBoundary>
  );
}

async function sidebarQueries(
  slug: string,
  activeOrganization: NonNullable<Awaited<ReturnType<typeof getOrganization>>>,
) {
  const queryClient = getQueryClient();
  queryClient.setQueryData(
    organization.queries.get({ slug }).queryKey,
    activeOrganization,
  );
  queryClient.setQueryData(
    organization.queries.get({ id: activeOrganization.id }).queryKey,
    activeOrganization,
  );
  await queryClient.query({
    ...billing.queries.summary.get(activeOrganization.id),
    queryFn: () =>
      handle(api.billing.summary.$get, {
        schema: getBillingSummaryResponseSchema,
      })({
        query: { referenceId: activeOrganization.id },
      }),
  });

  return dehydrate(queryClient);
}

async function AuthenticatedOrgActionBar({
  params,
}: {
  params: Promise<{ organization: string }>;
}) {
  const { items } = await getWorkspace(params);
  return <DashboardActionBar menu={items} />;
}
