import { notFound } from "next/navigation";

import { getOrganization } from "~/lib/auth/server";
import { getMetadata } from "~/lib/metadata";
import { CreditsPage } from "~/modules/credits/credits-page";

export const generateMetadata = getMetadata({
  title: "organization:credits.title",
  description: "organization:credits.description",
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

  return <CreditsPage organizationId={activeOrganization.id} />;
}
