import { getPersonalAccountForSession } from "~/lib/auth/server";
import { getMetadata } from "~/lib/metadata";
import { GettingStarted } from "~/modules/organization/home/getting-started";
import { UserOrganizationInvitationsBanner } from "~/modules/organization/invitations/user/user-organization-invitations";

export const generateMetadata = getMetadata({
  title: "common:home",
  description: "dashboard:organization.home.description",
});

export default async function OrganizationPage({
  params,
}: {
  params: Promise<{ organization: string }>;
}) {
  const { organization } = await params;
  const personal = await getPersonalAccountForSession();

  return (
    <>
      {/* invitations are the user's own: they show in the personal account */}
      {personal?.slug === organization && <UserOrganizationInvitationsBanner />}
      <GettingStarted organization={organization} />
    </>
  );
}
