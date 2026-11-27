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
  providerOf,
} from "~/modules/marketing/models/catalog";

export const generateMetadata = getMetadata({
  title: "marketing:models.title",
  description: "marketing:models.description",
  canonical: pathsConfig.marketing.models.index,
});

const first = (value: string | string[] | undefined) =>
  (Array.isArray(value) ? value[0] : value)?.trim() ?? "";

export default async function ModelsPage(props: {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const [searchParams, locale] = await Promise.all([
    props.searchParams,
    getLocale(),
  ]);
  const { t } = await getTranslation({ ns: "marketing", locale });
  const catalog = await getCatalog();

  const query = first(searchParams.q).toLowerCase();
  const provider = first(searchParams.provider);
  const providers = [...new Set((catalog ?? []).map(providerOf))].sort();
  const models = (catalog ?? []).filter(
    (model) =>
      (!query || model.id.toLowerCase().includes(query)) &&
      (!provider || providerOf(model) === provider),
  );

  return (
    <Section>
      <SectionHeader>
        <SectionTitle>{t("models.title")}</SectionTitle>
        <SectionDescription>{t("models.description")}</SectionDescription>
      </SectionHeader>

      {catalog === null ? (
        <p role="alert" className="text-muted-foreground">
          {t("models.unavailable")}
        </p>
      ) : (
        <div className="w-full max-w-5xl space-y-6">
          <form method="get" className="flex flex-wrap items-end gap-3">
            <div className="flex min-w-48 flex-1 flex-col gap-1.5">
              <label htmlFor="models-q" className="text-sm font-medium">
                {t("models.filters.search")}
              </label>
              <input
                id="models-q"
                name="q"
                type="search"
                defaultValue={first(searchParams.q)}
                placeholder={t("models.filters.searchPlaceholder")}
                className="border-input bg-background h-10 rounded-md border px-3 text-base"
              />
            </div>
            <div className="flex flex-col gap-1.5">
              <label htmlFor="models-provider" className="text-sm font-medium">
                {t("models.filters.provider")}
              </label>
              <select
                id="models-provider"
                name="provider"
                defaultValue={provider}
                className="border-input bg-background h-10 rounded-md border px-3 text-base"
              >
                <option value="">{t("models.filters.allProviders")}</option>
                {providers.map((name) => (
                  <option key={name} value={name}>
                    {name}
                  </option>
                ))}
              </select>
            </div>
            <button
              type="submit"
              className="bg-primary text-primary-foreground h-10 rounded-md px-4 text-sm font-medium"
            >
              {t("models.filters.apply")}
            </button>
          </form>

          <p className="text-muted-foreground text-sm" aria-live="polite">
            {t("models.count", { count: models.length })}
          </p>

          {models.length > 0 && (
            <div className="overflow-x-auto rounded-lg border">
              <table className="w-full text-left text-sm">
                <thead className="bg-muted/50">
                  <tr>
                    <th scope="col" className="px-4 py-3 font-medium">
                      {t("models.columns.model")}
                    </th>
                    <th
                      scope="col"
                      className="px-4 py-3 text-right font-medium"
                    >
                      {t("models.columns.context")}
                    </th>
                    <th
                      scope="col"
                      className="px-4 py-3 text-right font-medium"
                    >
                      {t("models.columns.input")}
                    </th>
                    <th
                      scope="col"
                      className="px-4 py-3 text-right font-medium"
                    >
                      {t("models.columns.output")}
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {models.map((model) => (
                    <tr key={model.id} className="border-t">
                      <th scope="row" className="px-4 py-3 font-normal">
                        <TurboLink
                          href={pathsConfig.marketing.models.model(model.id)}
                          className="font-mono underline-offset-4 hover:underline"
                        >
                          {model.id}
                        </TurboLink>
                      </th>
                      <td className="px-4 py-3 text-right tabular-nums">
                        {formatCount(model.context_length, locale) ?? "–"}
                      </td>
                      <td className="px-4 py-3 text-right whitespace-nowrap tabular-nums">
                        {perMillion(model, "prompt", locale)}
                      </td>
                      <td className="px-4 py-3 text-right whitespace-nowrap tabular-nums">
                        {perMillion(model, "completion", locale)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <p className="text-muted-foreground text-xs">
            {t("models.priceNote")}
          </p>
        </div>
      )}
    </Section>
  );
}
