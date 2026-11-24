import { notFound } from "next/navigation";

import { appConfig } from "~/config/app";
import { pathsConfig } from "~/config/paths";
import { getLocale, getTranslation } from "~/lib/i18n";
import { getMetadata } from "~/lib/metadata";
import { TurboLink } from "~/modules/common/turbo-link";
import {
  Section,
  SectionDescription,
  SectionHeader,
  SectionTitle,
} from "~/modules/marketing/layout/section";
import {
  formatCount,
  getCatalog,
  perMillion,
} from "~/modules/marketing/models/catalog";

interface PageProps {
  params: Promise<{ id: string[]; locale: string }>;
}

const findModel = async (segments: string[]) => {
  const id = segments.map((part) => decodeURIComponent(part)).join("/");
  const catalog = await getCatalog();
  return { id, catalog, model: catalog?.find((entry) => entry.id === id) };
};

export async function generateMetadata({ params }: PageProps) {
  const { id } = await findModel((await params).id);
  return getMetadata({
    title: id,
    description: "marketing:models.description",
    canonical: pathsConfig.marketing.models.model(id),
  })({ params });
}

export default async function ModelPage({ params }: PageProps) {
  const [{ id, catalog, model }, locale] = await Promise.all([
    findModel((await params).id),
    getLocale(),
  ]);
  const { t } = await getTranslation({ ns: "marketing", locale });

  if (catalog === null) {
    return (
      <Section>
        <SectionHeader>
          <SectionTitle>{id}</SectionTitle>
          <SectionDescription>{t("models.unavailable")}</SectionDescription>
        </SectionHeader>
      </Section>
    );
  }
  if (!model) {
    return notFound();
  }

  const apiUrl = `${appConfig.url}/api/v1`;
  const facts = [
    {
      label: t("models.columns.context"),
      value: formatCount(model.context_length, locale),
    },
    {
      label: t("models.detail.maxOutput"),
      value: formatCount(model.top_provider.max_completion_tokens, locale),
    },
    {
      label: t("models.detail.inputs"),
      value: model.architecture.input_modalities.join(", ") || null,
    },
    {
      label: t("models.detail.outputs"),
      value: model.architecture.output_modalities.join(", ") || null,
    },
    {
      label: t("models.columns.input"),
      value: perMillion(model.pricing.prompt, locale),
    },
    {
      label: t("models.columns.output"),
      value: perMillion(model.pricing.completion, locale),
    },
    {
      label: t("models.detail.cacheRead"),
      value:
        model.pricing.input_cache_read === undefined
          ? null
          : perMillion(model.pricing.input_cache_read, locale),
    },
    {
      label: t("models.detail.cacheWrite"),
      value:
        model.pricing.input_cache_write === undefined
          ? null
          : perMillion(model.pricing.input_cache_write, locale),
    },
  ].filter((fact) => fact.value !== null);

  const snippet = `curl ${apiUrl}/chat/completions \\
  -H "Authorization: Bearer $API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${model.id}",
    "messages": [{ "role": "user", "content": "Hello" }]
  }'`;

  return (
    <Section>
      <SectionHeader>
        <SectionTitle className="font-mono break-all">{model.id}</SectionTitle>
        <SectionDescription>
          {t("models.detail.description")}
        </SectionDescription>
      </SectionHeader>

      <div className="w-full max-w-3xl space-y-8">
        <dl className="grid grid-cols-[auto_1fr] gap-x-8 gap-y-3 rounded-lg border p-6 text-sm">
          {facts.map((fact) => (
            <div key={fact.label} className="contents">
              <dt className="text-muted-foreground">{fact.label}</dt>
              <dd className="tabular-nums">{fact.value}</dd>
            </div>
          ))}
        </dl>
        <p className="text-muted-foreground text-xs">{t("models.priceNote")}</p>

        <div className="space-y-2">
          <h2 className="text-lg font-semibold">{t("models.detail.use")}</h2>
          <pre className="bg-muted overflow-x-auto rounded-lg p-4 text-sm">
            <code>{snippet}</code>
          </pre>
          <p className="text-muted-foreground text-sm">
            {t("models.detail.sdkNote", { url: apiUrl })}
          </p>
        </div>

        <TurboLink
          href={pathsConfig.marketing.models.index}
          className="text-sm underline underline-offset-4"
        >
          {t("models.detail.back")}
        </TurboLink>
      </div>
    </Section>
  );
}
