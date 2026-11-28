import { getMetadata } from "~/lib/metadata";
import { PromoCodesList } from "~/modules/admin/promo-codes/promo-codes-list";

export const generateMetadata = getMetadata({
  title: "admin:promoCodes.header.title",
  description: "admin:promoCodes.header.description",
});

export default function PromoCodesPage() {
  return <PromoCodesList />;
}
