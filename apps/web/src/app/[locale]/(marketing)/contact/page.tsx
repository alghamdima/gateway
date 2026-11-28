import { getTranslation } from "~/lib/i18n";
import { getMetadata } from "~/lib/metadata";
import { ContactForm } from "~/modules/marketing/contact/contact-form";
import { topicFrom } from "~/modules/marketing/contact/utils/schema";
import {
  Section,
  SectionBadge,
  SectionDescription,
  SectionHeader,
  SectionTitle,
} from "~/modules/marketing/layout/section";

export const generateMetadata = getMetadata({
  title: "marketing:contact.label",
  description: "marketing:contact.description",
});

export default async function ContactPage({
  searchParams,
}: {
  searchParams: Promise<{ topic?: string }>;
}) {
  const [{ t }, { topic }] = await Promise.all([
    getTranslation({ ns: "marketing" }),
    searchParams,
  ]);
  return (
    <Section>
      <SectionHeader>
        <SectionBadge>{t("contact.label")}</SectionBadge>
        <SectionTitle>{t("contact.title")}</SectionTitle>
        <SectionDescription>{t("contact.description")}</SectionDescription>
      </SectionHeader>

      <ContactForm topic={topicFrom(topic)} />
    </Section>
  );
}
