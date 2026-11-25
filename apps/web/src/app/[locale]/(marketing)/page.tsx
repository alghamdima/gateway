import { getMetadata } from "~/lib/metadata";
import { Faq } from "~/modules/marketing/home/faq";
import { Features } from "~/modules/marketing/home/features";
import { Hero } from "~/modules/marketing/home/hero";

export const generateMetadata = getMetadata({
  title: "marketing:home.seo.title",
  description: "marketing:product.description",
});

export default function HomePage() {
  return (
    <>
      <Hero />
      <Features />
      <Faq />
    </>
  );
}
