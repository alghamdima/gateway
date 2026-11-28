import { getMetadata } from "~/lib/metadata";
import { ContactRequestsList } from "~/modules/admin/contact-requests/contact-requests-list";

export const generateMetadata = getMetadata({
  title: "admin:contactRequests.header.title",
  description: "admin:contactRequests.header.description",
});

export default function ContactRequestsPage() {
  return <ContactRequestsList />;
}
