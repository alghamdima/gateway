"use client";

import { useMutation } from "@tanstack/react-query";
import { useRouter } from "next/navigation";
import { useEffect, useRef, useState } from "react";

import { handle } from "@workspace/api/utils";
import { MemberRole } from "@workspace/auth";
import { useTranslation } from "@workspace/i18n";
import { cn } from "@workspace/ui";
import { Button } from "@workspace/ui-web/button";
import { Field, FieldDescription, FieldLabel } from "@workspace/ui-web/field";
import { Icons } from "@workspace/ui-web/icons";
import { Input } from "@workspace/ui-web/input";
import { RadioGroup, RadioGroupItem } from "@workspace/ui-web/radio-group";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { Spinner } from "@workspace/ui-web/spinner";
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@workspace/ui-web/tabs";
import { toast } from "@workspace/ui-web/toast";

import { pathsConfig } from "~/config/paths";
import { api } from "~/lib/api/client";
import { useCopy } from "~/modules/common/hooks/use-copy";
import { formatMicroSar } from "~/modules/keys/money";
import { organization as organizationApi } from "~/modules/organization/lib/api";

type AccountType = "individual" | "organization";
type Step = "welcome" | "organization" | "ready";

interface Account {
  id: string;
  slug: string;
}

interface OnboardingWizardProps {
  /** The user's personal account: where an individual starts. */
  readonly personal: Account;
  /** The public address of the API, for the first-request snippet. */
  readonly apiUrl: string;
  /** A model the catalog offers now, for the snippet; null when there is none. */
  readonly model: string | null;
}

/** The languages of the first-request snippet; their names are not translated. */
const SNIPPET_LANGUAGES = [
  { id: "curl", name: "cURL" },
  { id: "typescript", name: "TypeScript" },
  { id: "python", name: "Python" },
] as const;

const EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

/**
 * The welcome wizard a new user sees once (ONB-03): what the product is and
 * "individual or organization", the organization's name and first invitations
 * if chosen, then a starter API key and a first request. The personal account
 * exists either way (ADR 0020); the choice decides where they start.
 */
export const OnboardingWizard = ({
  personal,
  apiUrl,
  model,
}: OnboardingWizardProps) => {
  const { t } = useTranslation("common");
  const router = useRouter();
  const [step, setStep] = useState<Step>("welcome");
  const [accountType, setAccountType] = useState<AccountType>("individual");
  // set once the organization exists: going back does not create it again
  const [created, setCreated] = useState<Account | null>(null);

  const steps: Step[] =
    accountType === "organization"
      ? ["welcome", "organization", "ready"]
      : ["welcome", "ready"];
  const target = accountType === "organization" && created ? created : personal;

  // Minted once, when the last step is first reached, and kept here: the
  // value is shown only now, so going back and forth must not lose it.
  const requested = useRef(false);
  const starter = useMutation({
    mutationFn: (organizationId: string) =>
      handle(api.onboarding["starter-key"].$post)({
        json: { organizationId },
      }),
    // shown in the step itself (this replaces the default handler)
    onError: () => undefined,
  });
  const mintStarterKey = starter.mutate;
  useEffect(() => {
    if (step === "ready" && !requested.current) {
      requested.current = true;
      mintStarterKey(target.id);
    }
  }, [step, target.id, mintStarterKey]);

  const finish = useMutation({
    mutationFn: () =>
      handle(api.onboarding.complete.$post)({ json: { accountType } }),
    onSuccess: () => {
      router.replace(pathsConfig.dashboard.organization(target.slug).index);
      router.refresh();
    },
  });

  return (
    <div className="flex w-full flex-col gap-6">
      <section className="bg-card rounded-xl border p-6 sm:p-8">
        {step === "welcome" && (
          <WelcomeStep
            accountType={accountType}
            onChange={setAccountType}
            // the starter key lives in the account it was created in
            isLocked={!starter.isIdle}
          />
        )}
        {step === "organization" && (
          <OrganizationStep
            created={created}
            onCreated={(account) => {
              setCreated(account);
              setStep("ready");
            }}
            onBack={() => setStep("welcome")}
          />
        )}
        {step === "ready" && (
          <ReadyStep
            isOrganization={target.id !== personal.id}
            apiUrl={apiUrl}
            model={model}
            starter={
              starter.isSuccess
                ? starter.data
                : starter.isError
                  ? null
                  : undefined
            }
            // why there is no key to show: the API's own reason (it exists already, or the gateway is away)
            starterError={starter.error?.message}
          />
        )}
      </section>

      {step !== "organization" && (
        <footer className="flex items-center justify-between gap-4">
          <div className="w-24">
            {step === "ready" && (
              <Button
                variant="ghost"
                onClick={() => setStep(steps.at(-2) ?? "welcome")}
                disabled={finish.isPending}
              >
                <Icons.ArrowLeft />
                {t("onboarding.back")}
              </Button>
            )}
          </div>
          <ProgressDots steps={steps} current={step} />
          <div className="flex w-24 justify-end">
            {step === "welcome" ? (
              <Button onClick={() => setStep(steps[1] ?? "ready")}>
                {t("onboarding.next")}
              </Button>
            ) : (
              <Button
                onClick={() => finish.mutate()}
                disabled={finish.isPending}
              >
                {finish.isPending ? <Spinner /> : t("onboarding.finish")}
              </Button>
            )}
          </div>
        </footer>
      )}
    </div>
  );
};

const ProgressDots = ({
  steps,
  current,
}: {
  readonly steps: Step[];
  readonly current: Step;
}) => {
  const { t } = useTranslation("common");
  const index = steps.indexOf(current);

  return (
    <ol
      className="flex items-center gap-2"
      aria-label={t("onboarding.progress", {
        current: index + 1,
        total: steps.length,
      })}
    >
      {steps.map((name, position) => (
        <li
          key={name}
          aria-current={position === index ? "step" : undefined}
          className={cn(
            "size-2 rounded-full",
            position === index ? "bg-primary" : "bg-muted-foreground/30",
          )}
        />
      ))}
    </ol>
  );
};

const WelcomeStep = ({
  accountType,
  onChange,
  isLocked,
}: {
  readonly accountType: AccountType;
  readonly onChange: (type: AccountType) => void;
  /** The choice is made: the starter key was created in that account. */
  readonly isLocked: boolean;
}) => {
  const { t } = useTranslation("common");
  const tiles = [
    { id: "access", icon: <Icons.Key className="size-4" /> },
    { id: "policy", icon: <Icons.Globe2 className="size-4" /> },
    { id: "prepaid", icon: <Icons.HandCoins className="size-4" /> },
  ] as const;
  const choices = [
    { value: "individual", icon: <Icons.UserRound className="size-5" /> },
    { value: "organization", icon: <Icons.Building className="size-5" /> },
  ] as const;

  return (
    <div className="flex flex-col gap-8">
      <header className="flex flex-col gap-2">
        <h1 className="text-2xl font-semibold tracking-tight">
          {t("onboarding.welcome.title")}
        </h1>
        <p className="text-muted-foreground">
          {t("onboarding.welcome.description")}
        </p>
      </header>

      <ul className="grid gap-4 sm:grid-cols-3">
        {tiles.map((tile) => (
          <li
            key={tile.id}
            className="flex flex-col gap-2 rounded-lg border p-4"
          >
            {tile.icon}
            <span className="font-medium">
              {t(`onboarding.welcome.tiles.${tile.id}.title`)}
            </span>
            <span className="text-muted-foreground text-sm text-pretty">
              {t(`onboarding.welcome.tiles.${tile.id}.description`)}
            </span>
          </li>
        ))}
      </ul>

      <div className="flex flex-col gap-3">
        <div>
          <h2 id="onboarding-account-type" className="font-medium">
            {t("onboarding.welcome.question")}
          </h2>
          <p className="text-muted-foreground text-sm">
            {t(
              isLocked
                ? "onboarding.welcome.locked"
                : "onboarding.welcome.changeLater",
            )}
          </p>
        </div>
        <RadioGroup
          aria-labelledby="onboarding-account-type"
          value={accountType}
          disabled={isLocked}
          onValueChange={(value) =>
            onChange(value === "organization" ? "organization" : "individual")
          }
          className="grid gap-4 sm:grid-cols-2"
        >
          {choices.map((choice) => (
            <label
              key={choice.value}
              className={cn(
                "flex cursor-pointer items-center gap-4 rounded-lg border p-4 transition-colors",
                accountType === choice.value
                  ? "border-primary bg-accent/50"
                  : "hover:bg-accent/30",
              )}
            >
              <span className="bg-muted flex size-10 shrink-0 items-center justify-center rounded-full">
                {choice.icon}
              </span>
              <span className="flex flex-1 flex-col gap-1">
                <span className="font-medium">
                  {t(`onboarding.welcome.${choice.value}.title`)}
                </span>
                <span className="text-muted-foreground text-sm text-pretty">
                  {t(`onboarding.welcome.${choice.value}.description`)}
                </span>
              </span>
              <RadioGroupItem value={choice.value} className="self-start" />
            </label>
          ))}
        </RadioGroup>
      </div>
    </div>
  );
};

const OrganizationStep = ({
  created,
  onCreated,
  onBack,
}: {
  readonly created: Account | null;
  readonly onCreated: (account: Account) => void;
  readonly onBack: () => void;
}) => {
  const { t } = useTranslation("common");
  const [name, setName] = useState("");
  const [emails, setEmails] = useState([""]);
  const [error, setError] = useState<string | null>(null);

  const getSlug = useMutation(organizationApi.mutations.getSlug);
  const create = useMutation(organizationApi.mutations.create);
  const invite = useMutation(organizationApi.mutations.members.invite);

  const setUp = useMutation({
    mutationFn: async () => {
      const invited = emails.map((email) => email.trim()).filter(Boolean);
      if (!created && name.trim().length < 2) {
        throw new Error(t("onboarding.organization.name.invalid"));
      }
      if (invited.some((email) => !EMAIL.test(email))) {
        throw new Error(t("onboarding.organization.invite.invalid"));
      }
      let account = created;
      if (!account) {
        const { slug } = await getSlug.mutateAsync({
          query: { name: name.trim() },
        });
        const organization = await create.mutateAsync({
          name: name.trim(),
          slug,
        });
        account = { id: organization.id, slug: organization.slug };
      }
      // an invitation that fails does not undo the organization: it can be sent again from Members
      const failed: string[] = [];
      for (const email of invited) {
        const result = await invite
          .mutateAsync({
            email,
            role: MemberRole.MEMBER,
            organizationId: account.id,
          })
          .catch(() => ({ error: true }));
        if (result && "error" in result && result.error) {
          failed.push(email);
        }
      }
      return { account, failed };
    },
    onSuccess: ({ account, failed }) => {
      if (failed.length > 0) {
        toast.add({
          title: t("onboarding.organization.invite.failed", {
            emails: failed.join(", "),
          }),
          type: "error",
        });
      }
      onCreated(account);
    },
    // say why here, beside the form (this replaces the default handler)
    onError: (failure: Error) => setError(failure.message),
  });

  return (
    <form
      noValidate
      className="flex flex-col gap-8"
      onSubmit={(event) => {
        event.preventDefault();
        setError(null);
        setUp.mutate();
      }}
    >
      <header className="flex flex-col gap-2">
        <h1 className="text-2xl font-semibold tracking-tight">
          {t("onboarding.organization.title")}
        </h1>
        <p className="text-muted-foreground">
          {t("onboarding.organization.description")}
        </p>
      </header>

      <Field>
        <FieldLabel htmlFor="onboarding-organization-name">
          {t("onboarding.organization.name.label")}
        </FieldLabel>
        <Input
          id="onboarding-organization-name"
          placeholder={t("onboarding.organization.name.placeholder")}
          value={name}
          onChange={(event) => setName(event.target.value)}
          disabled={!!created || setUp.isPending}
          autoFocus
        />
        {created && (
          <FieldDescription>
            {t("onboarding.organization.name.created")}
          </FieldDescription>
        )}
      </Field>

      <fieldset className="flex flex-col gap-3">
        <legend className="mb-1 text-sm font-medium">
          {t("onboarding.organization.invite.label")}
        </legend>
        <p className="text-muted-foreground text-sm">
          {t("onboarding.organization.invite.description")}
        </p>
        {emails.map((email, index) => (
          <Input
            // the rows have no identity but their place
            // eslint-disable-next-line react/no-array-index-key
            key={index}
            type="email"
            inputMode="email"
            aria-label={t("onboarding.organization.invite.email", {
              number: index + 1,
            })}
            placeholder={t("onboarding.organization.invite.placeholder")}
            value={email}
            onChange={(event) =>
              setEmails(
                emails.map((current, position) =>
                  position === index ? event.target.value : current,
                ),
              )
            }
            disabled={setUp.isPending}
          />
        ))}
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="self-start"
          onClick={() => setEmails([...emails, ""])}
          disabled={setUp.isPending || emails.length >= 10}
        >
          <Icons.Plus />
          {t("onboarding.organization.invite.add")}
        </Button>
      </fieldset>

      {error && (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}

      <div className="flex items-center justify-between">
        <Button
          type="button"
          variant="ghost"
          onClick={onBack}
          disabled={setUp.isPending}
        >
          <Icons.ArrowLeft />
          {t("onboarding.back")}
        </Button>
        <Button type="submit" disabled={setUp.isPending}>
          {setUp.isPending ? <Spinner /> : t("continue")}
        </Button>
      </div>
    </form>
  );
};

interface StarterKey {
  key: string;
  limitMicroSar: number | null;
  expiresAt: string | null;
}

const ReadyStep = ({
  isOrganization,
  apiUrl,
  model,
  starter,
  starterError,
}: {
  readonly isOrganization: boolean;
  readonly apiUrl: string;
  readonly model: string | null;
  /** The starter key; undefined while it is being created, null when it could not be. */
  readonly starter: StarterKey | null | undefined;
  readonly starterError?: string;
}) => {
  const { t, i18n } = useTranslation("common");
  const key = starter?.key;
  const shown = key ?? "$LEMRU_API_KEY";
  const modelId = model ?? "provider/model";
  const snippets = {
    curl: `curl ${apiUrl}/chat/completions \\
  -H "Authorization: Bearer ${shown}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${modelId}",
    "messages": [{ "role": "user", "content": "Hello" }]
  }'`,
    typescript: `const response = await fetch("${apiUrl}/chat/completions", {
  method: "POST",
  headers: {
    Authorization: "Bearer ${shown}",
    "Content-Type": "application/json",
  },
  body: JSON.stringify({
    model: "${modelId}",
    messages: [{ role: "user", content: "Hello" }],
  }),
});

console.log(await response.json());`,
    python: `import requests

response = requests.post(
    "${apiUrl}/chat/completions",
    headers={"Authorization": "Bearer ${shown}"},
    json={
        "model": "${modelId}",
        "messages": [{"role": "user", "content": "Hello"}],
    },
)
print(response.json())`,
  } as const;

  const limit = starter?.limitMicroSar
    ? t("onboarding.ready.key.limit", {
        amount: formatMicroSar(starter.limitMicroSar, i18n.language),
      })
    : null;
  const expiry = starter?.expiresAt
    ? t("onboarding.ready.key.expires", {
        date: new Date(starter.expiresAt).toLocaleDateString(i18n.language, {
          dateStyle: "medium",
        }),
      })
    : null;

  return (
    <div className="flex flex-col gap-8">
      <header className="flex flex-col gap-2">
        <h1 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          <Icons.CircleCheck className="text-primary size-6" />
          {t(
            isOrganization
              ? "onboarding.ready.titleOrganization"
              : "onboarding.ready.title",
          )}
        </h1>
        <p className="text-muted-foreground">
          {t("onboarding.ready.description")}
        </p>
      </header>

      <div className="flex flex-col gap-2">
        <h2 className="font-medium">{t("onboarding.ready.key.label")}</h2>
        {starter === undefined ? (
          <Skeleton className="h-10 w-full" />
        ) : key ? (
          <>
            <CopyBlock text={key} label={t("onboarding.ready.key.copy")} />
            <p className="text-muted-foreground text-sm">
              {[t("onboarding.ready.key.once"), limit, expiry]
                .filter(Boolean)
                .join(" ")}
            </p>
          </>
        ) : (
          <p role="status" className="text-sm">
            {starterError ?? t("onboarding.ready.key.unavailable")}
          </p>
        )}
      </div>

      <div className="flex flex-col gap-2">
        <h2 className="font-medium">{t("onboarding.ready.request.label")}</h2>
        <Tabs defaultValue="curl">
          <TabsList>
            {SNIPPET_LANGUAGES.map((language) => (
              <TabsTrigger key={language.id} value={language.id}>
                {language.name}
              </TabsTrigger>
            ))}
          </TabsList>
          {SNIPPET_LANGUAGES.map(({ id }) => (
            <TabsContent key={id} value={id}>
              <CopyBlock
                text={snippets[id]}
                label={t("onboarding.ready.request.copy")}
                isMultiline
              />
            </TabsContent>
          ))}
        </Tabs>
        <p className="text-muted-foreground text-sm">
          {t("onboarding.ready.request.note")}
        </p>
      </div>
    </div>
  );
};

const CopyBlock = ({
  text,
  label,
  isMultiline = false,
}: {
  readonly text: string;
  readonly label: string;
  readonly isMultiline?: boolean;
}) => {
  const { copied, copy } = useCopy();

  return (
    <div className="bg-muted relative rounded-lg border">
      <pre
        className={cn(
          "overflow-x-auto p-3 pr-12 font-mono text-sm",
          isMultiline ? "whitespace-pre" : "break-all whitespace-pre-wrap",
        )}
      >
        <code>{text}</code>
      </pre>
      <Button
        type="button"
        variant="ghost"
        size="icon"
        className="absolute top-1.5 right-1.5"
        aria-label={label}
        onClick={() => void copy(text)}
      >
        {copied ? <Icons.Check /> : <Icons.Copy />}
      </Button>
    </div>
  );
};
