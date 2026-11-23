import { notFound } from "next/navigation";

import { getOrganization } from "~/lib/auth/server";
import { getMetadata } from "~/lib/metadata";
import { KeysPage } from "~/modules/keys/keys-page";

export const generateMetadata = getMetadata({
  title: "organization:keys.title",
  description: "organization:keys.description",
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

  return <KeysPage organizationId={activeOrganization.id} />;
}
