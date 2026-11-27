import { getMetadata } from "~/lib/metadata";
import { ModelPrices } from "~/modules/admin/model-prices/model-prices";

export const generateMetadata = getMetadata({
  title: "admin:modelPrices.header.title",
  description: "admin:modelPrices.header.description",
});

export default function ModelPricesPage() {
  return <ModelPrices />;
}
