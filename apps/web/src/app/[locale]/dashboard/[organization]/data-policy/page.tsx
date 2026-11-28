import { notFound } from "next/navigation";

import { getOrganization } from "~/lib/auth/server";
import { getMetadata } from "~/lib/metadata";
import { DataPolicyPage } from "~/modules/data-policy/data-policy-page";

export const generateMetadata = getMetadata({
  title: "organization:dataPolicy.title",
  description: "organization:dataPolicy.description",
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

  return <DataPolicyPage organizationId={activeOrganization.id} />;
}
