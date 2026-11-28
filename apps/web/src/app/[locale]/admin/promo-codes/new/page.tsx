import { getMetadata } from "~/lib/metadata";
import { PromoCodeForm } from "~/modules/admin/promo-codes/promo-code-form";

export const generateMetadata = getMetadata({
  title: "admin:promoCodes.form.createTitle",
});

export default function NewPromoCodePage() {
  return <PromoCodeForm />;
}
