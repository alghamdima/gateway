import {
  Accordion,
  AccordionContent,
  AccordionItem,
  AccordionTrigger,
} from "@workspace/ui-web/accordion";
import { buttonVariants } from "@workspace/ui-web/button";
import { Icons } from "@workspace/ui-web/icons";

import { pathsConfig } from "~/config/paths";
import { getTranslation } from "~/lib/i18n";
import { TurboLink } from "~/modules/common/turbo-link";
import {
  Section,
  SectionHeader,
  SectionBadge,
  SectionTitle,
  SectionDescription,
} from "~/modules/marketing/layout/section";

const questions = [
  {
    question: "faq.question.whatIsIt.question",
    answer: "faq.question.whatIsIt.answer",
  },
  {
    question: "faq.question.billing.question",
    answer: "faq.question.billing.answer",
  },
  {
    question: "faq.question.compatibility.question",
    answer: "faq.question.compatibility.answer",
  },
  {
    question: "faq.question.dataLocation.question",
    answer: "faq.question.dataLocation.answer",
  },
  {
    question: "faq.question.evidence.question",
    answer: "faq.question.evidence.answer",
  },
] as const;

export const Faq = async () => {
  const { t } = await getTranslation({ ns: "marketing" });

  return (
    <Section id="faq" className="lg:flex-row lg:items-start">
      <SectionHeader className="grow basis-0 lg:items-start">
        <SectionBadge>{t("faq.label")}</SectionBadge>
        <SectionTitle className="lg:text-left">{t("faq.title")}</SectionTitle>
        <SectionDescription className="lg:text-left">
          {t("faq.description")}
        </SectionDescription>

        <TurboLink
          href={pathsConfig.marketing.contact}
          className={buttonVariants({
            variant: "outline",
          })}
        >
          {t("faq.cta")}
          <Icons.ArrowRight className="ml-1 size-4" />
        </TurboLink>
      </SectionHeader>

      <Accordion multiple className="grow basis-0">
        {questions.map((question) => (
          <AccordionItem key={question.question} value={question.question}>
            <AccordionTrigger className="text-base">
              {t(question.question)}
            </AccordionTrigger>
            <AccordionContent className="text-base">
              {t(question.answer)}
            </AccordionContent>
          </AccordionItem>
        ))}
      </Accordion>
    </Section>
  );
};
