import { getMetadata } from "~/lib/metadata";
import { Providers } from "~/modules/admin/providers/providers";

export const generateMetadata = getMetadata({
  title: "admin:providers.header.title",
  description: "admin:providers.header.description",
});

export default function ProvidersPage() {
  return <Providers />;
}
