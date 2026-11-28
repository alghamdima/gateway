import { getMetadata } from "~/lib/metadata";
import { Payments } from "~/modules/admin/payments/payments";

export const generateMetadata = getMetadata({
  title: "admin:payments.header.title",
  description: "admin:payments.header.description",
});

export default function PaymentsPage() {
  return <Payments />;
}
