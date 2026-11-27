import { getMetadata } from "~/lib/metadata";
import { PromoCodeDetail } from "~/modules/admin/promo-codes/promo-code-detail";

export const generateMetadata = getMetadata({
  title: "admin:promoCodes.header.title",
});

export default async function PromoCodePage({
  params,
}: {
  params: Promise<{ id: string }>;
}) {
  const { id } = await params;
  return <PromoCodeDetail id={id} />;
}
