import { getMetadata } from "~/lib/metadata";
import { Pricing } from "~/modules/admin/pricing/pricing";

export const generateMetadata = getMetadata({
  title: "admin:pricing.header.title",
  description: "admin:pricing.header.description",
});

export default function PricingPage() {
  return <Pricing />;
}
