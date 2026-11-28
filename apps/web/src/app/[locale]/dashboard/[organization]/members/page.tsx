import { notFound, redirect } from "next/navigation";

import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@workspace/ui-web/tabs";

import { pathsConfig } from "~/config/paths";
import {
  getOrganization,
  getPersonalAccountForSession,
} from "~/lib/auth/server";
import { getTranslation } from "~/lib/i18n";
import { getMetadata } from "~/lib/metadata";
import { InvitationsDataTable } from "~/modules/organization/invitations/data-table/invitations-data-table";
import { MembersDataTable } from "~/modules/organization/members/data-table/members-data-table";
import { InviteMember } from "~/modules/organization/members/invite-member";

export const generateMetadata = getMetadata({
  title: "organization:members.title",
  description: "organization:members.header.description",
});

export default async function MembersPage({
  params,
}: {
  params: Promise<{ organization: string }>;
}) {
  const { organization } = await params;
  const activeOrganization = await getOrganization({ slug: organization });

  if (!activeOrganization) {
    return notFound();
  }

  // a personal account has no members and no organization settings (ADR 0020)
  if ((await getPersonalAccountForSession())?.id === activeOrganization.id) {
    return redirect(pathsConfig.dashboard.organization(organization).index);
  }

  const { t } = await getTranslation({ ns: "organization" });

  return (
    <>
      <InviteMember organizationId={activeOrganization.id} />

      <Tabs defaultValue="members" className="mt-4 w-full">
        <TabsList>
          <TabsTrigger value="members">{t("members.title")}</TabsTrigger>
          <TabsTrigger value="invitations">
            {t("invitations.title")}
          </TabsTrigger>
        </TabsList>

        <TabsContent value="members">
          <MembersDataTable organizationId={activeOrganization.id} />
        </TabsContent>
        <TabsContent value="invitations">
          <InvitationsDataTable organizationId={activeOrganization.id} />
        </TabsContent>
      </Tabs>
    </>
  );
}
