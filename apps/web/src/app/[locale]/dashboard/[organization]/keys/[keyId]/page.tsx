import { notFound } from "next/navigation";

import { getOrganization } from "~/lib/auth/server";
import { getMetadata } from "~/lib/metadata";
import { KeyPage } from "~/modules/keys/key-page";

export const generateMetadata = getMetadata({
  title: "organization:keys.title",
  description: "organization:keys.description",
});

/** One key's own page (KEYS-11): details, usage and its danger zone. */
export default async function Page({
  params,
}: {
  params: Promise<{ organization: string; keyId: string }>;
}) {
  const { organization, keyId } = await params;
  const activeOrganization = await getOrganization({ slug: organization });

  if (!activeOrganization) {
    return notFound();
  }

  return (
    <KeyPage
      organizationId={activeOrganization.id}
      organizationSlug={organization}
      keyId={keyId}
    />
  );
}
