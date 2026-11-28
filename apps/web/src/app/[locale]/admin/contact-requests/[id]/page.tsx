import { getMetadata } from "~/lib/metadata";
import { ContactRequestDetail } from "~/modules/admin/contact-requests/contact-request-detail";

export const generateMetadata = getMetadata({
  title: "admin:contactRequests.header.title",
});

export default async function ContactRequestPage({
  params,
}: {
  params: Promise<{ id: string }>;
}) {
  const { id } = await params;
  return <ContactRequestDetail id={id} />;
}
