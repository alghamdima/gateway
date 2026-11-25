import { getMetadata } from "~/lib/metadata";
import { GettingStarted } from "~/modules/organization/home/getting-started";

export const generateMetadata = getMetadata({
  title: "common:home",
  description: "dashboard:organization.home.description",
});

export default async function OrganizationPage({
  params,
}: {
  params: Promise<{ organization: string }>;
}) {
  return <GettingStarted organization={(await params).organization} />;
}
