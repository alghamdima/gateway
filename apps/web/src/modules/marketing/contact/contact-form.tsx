"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { Controller, useForm } from "react-hook-form";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Button } from "@workspace/ui-web/button";
import { Card, CardContent } from "@workspace/ui-web/card";
import { Field, FieldLabel, FieldError } from "@workspace/ui-web/field";
import { Input } from "@workspace/ui-web/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@workspace/ui-web/select";
import { Spinner } from "@workspace/ui-web/spinner";
import { Textarea } from "@workspace/ui-web/textarea";
import { toast } from "@workspace/ui-web/toast";

import { api } from "~/lib/api/client";

import {
  CONTACT_COMPANY_MAX,
  CONTACT_MESSAGE_MAX,
  CONTACT_NAME_MAX,
  CONTACT_TOPICS,
  contactRequestSchema,
} from "./utils/schema";

import type { ContactFormPayload, ContactTopic } from "./utils/schema";

export function ContactForm({ topic }: { topic: ContactTopic }) {
  const { t } = useTranslation(["common", "marketing"]);
  const form = useForm({
    resolver: standardSchemaResolver(contactRequestSchema),
    defaultValues: {
      topic,
      name: "",
      email: "",
      company: "",
      message: "",
    },
  });
  const topics = CONTACT_TOPICS.map((value) => ({
    value,
    label: t(`contact.form.topic.options.${value}`),
  }));

  const onSubmit = async (data: ContactFormPayload) => {
    try {
      await handle(api.site.contact.$post)({ json: data });
    } catch (error) {
      return toast.add({
        title: error instanceof Error ? error.message : t("error.general"),
        type: "error",
      });
    }

    toast.add({
      title: t("contact.form.success.title"),
      description: t("contact.form.success.description"),
      type: "success",
    });

    form.reset({ ...form.getValues(), message: "" });
  };

  return (
    <Card className="w-full max-w-lg border-none bg-transparent shadow-none">
      <CardContent className="w-full p-0">
        <form
          id="contact-form"
          onSubmit={form.handleSubmit(onSubmit)}
          className="flex flex-col gap-6"
        >
          <Controller
            name="topic"
            control={form.control}
            render={({ field, fieldState }) => (
              <Field data-invalid={fieldState.invalid}>
                <FieldLabel htmlFor="contact-form-topic">
                  {t("contact.form.topic.label")}
                </FieldLabel>
                <Select
                  items={topics}
                  onValueChange={field.onChange}
                  value={field.value}
                >
                  <SelectTrigger
                    id="contact-form-topic"
                    className="w-full"
                    aria-invalid={fieldState.invalid}
                  >
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {topics.map((option) => (
                      <SelectItem key={option.value} value={option.value}>
                        {option.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {fieldState.invalid && (
                  <FieldError errors={[fieldState.error]} />
                )}
              </Field>
            )}
          />

          <Controller
            name="name"
            control={form.control}
            render={({ field, fieldState }) => (
              <Field data-invalid={fieldState.invalid}>
                <FieldLabel htmlFor="contact-form-name">{t("name")}</FieldLabel>
                <Input
                  {...field}
                  id="contact-form-name"
                  autoComplete="name"
                  spellCheck={false}
                  aria-invalid={fieldState.invalid}
                  placeholder={t("contact.form.name.placeholder")}
                  maxLength={CONTACT_NAME_MAX}
                  required
                />
                {fieldState.invalid && (
                  <FieldError errors={[fieldState.error]} />
                )}
              </Field>
            )}
          />

          <Controller
            name="email"
            control={form.control}
            render={({ field, fieldState }) => (
              <Field data-invalid={fieldState.invalid}>
                <FieldLabel htmlFor="contact-form-email">
                  {t("email")}
                </FieldLabel>
                <Input
                  {...field}
                  type="email"
                  autoComplete="email"
                  inputMode="email"
                  spellCheck={false}
                  required
                  id="contact-form-email"
                  aria-invalid={fieldState.invalid}
                  placeholder={t("contact.form.email.placeholder")}
                />
                {fieldState.invalid && (
                  <FieldError errors={[fieldState.error]} />
                )}
              </Field>
            )}
          />

          <Controller
            name="company"
            control={form.control}
            render={({ field, fieldState }) => (
              <Field data-invalid={fieldState.invalid}>
                <FieldLabel
                  className="flex justify-between"
                  htmlFor="contact-form-company"
                >
                  <span>{t("contact.form.company.label")}</span>
                  <span className="text-muted-foreground text-xs">
                    {t("contact.form.company.optional")}
                  </span>
                </FieldLabel>
                <Input
                  {...field}
                  value={field.value ?? ""}
                  id="contact-form-company"
                  autoComplete="organization"
                  aria-invalid={fieldState.invalid}
                  placeholder={t("contact.form.company.placeholder")}
                  maxLength={CONTACT_COMPANY_MAX}
                />
                {fieldState.invalid && (
                  <FieldError errors={[fieldState.error]} />
                )}
              </Field>
            )}
          />

          <Controller
            name="message"
            control={form.control}
            render={({ field, fieldState }) => (
              <Field data-invalid={fieldState.invalid}>
                <FieldLabel
                  className="flex justify-between"
                  htmlFor="contact-form-message"
                >
                  <span>{t("message")}</span>
                  <span className="text-muted-foreground text-xs">
                    {field.value.length}/{CONTACT_MESSAGE_MAX}
                  </span>
                </FieldLabel>
                <Textarea
                  {...field}
                  id="contact-form-message"
                  aria-invalid={fieldState.invalid}
                  placeholder={t("contact.form.message.placeholder")}
                  maxLength={CONTACT_MESSAGE_MAX}
                  required
                />
                {fieldState.invalid && (
                  <FieldError errors={[fieldState.error]} />
                )}
              </Field>
            )}
          />

          <Button
            type="submit"
            className="mt-2"
            disabled={form.formState.isSubmitting}
          >
            {form.formState.isSubmitting ? (
              <Spinner className="size-5" />
            ) : (
              t("contact.form.submit")
            )}
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}
