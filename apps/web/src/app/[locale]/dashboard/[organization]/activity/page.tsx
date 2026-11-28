import { notFound } from "next/navigation";

import { getOrganization } from "~/lib/auth/server";
import { getMetadata } from "~/lib/metadata";
import { ActivityPage } from "~/modules/activity/activity-page";

export const generateMetadata = getMetadata({
  title: "organization:activity.title",
  description: "organization:activity.description",
});

export default async function Page({
  params,
}: {
  params: Promise<{ organization: string }>;
}) {
  const { organization } = await params;
  const activeOrganization = await getOrganization({ slug: organization });

  if (!activeOrganization) {
    return notFound();
  }

  return <ActivityPage organizationId={activeOrganization.id} />;
}
