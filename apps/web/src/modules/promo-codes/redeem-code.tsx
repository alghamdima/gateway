"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Button } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import { Field, FieldError, FieldLabel } from "@workspace/ui-web/field";
import { Input } from "@workspace/ui-web/input";
import { Spinner } from "@workspace/ui-web/spinner";
import { toast } from "@workspace/ui-web/toast";

import { api } from "~/lib/api/client";
import { formatMicroSar } from "~/modules/keys/money";

interface RedeemCodeProps {
  readonly organizationId: string;
}

/**
 * Redeems a free-credit promo code for the organization (BILL-08). The API
 * checks every rule and grants the credit at once; a refusal comes back with
 * its reason, translated by the server.
 */
export const RedeemCode = ({ organizationId }: RedeemCodeProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const queryClient = useQueryClient();
  const [code, setCode] = useState("");

  const redeem = useMutation({
    mutationFn: (value: string) =>
      handle(api["promo-codes"][":organizationId"].redeem.$post)({
        param: { organizationId },
        json: { code: value },
      }),
    onSuccess: async (result) => {
      setCode("");
      await queryClient.invalidateQueries({
        queryKey: ["credits", organizationId],
      });
      toast.add({
        title: t("organization:credits.redeem.success", {
          credit: formatMicroSar(result.creditMicroSar, i18n.language),
          code: result.code,
        }),
        type: "success",
      });
    },
  });

  const submit = () => {
    if (code.trim()) {
      redeem.mutate(code.trim());
    }
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("organization:credits.redeem.title")}</CardTitle>
        <CardDescription>
          {t("organization:credits.redeem.description")}
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form
          noValidate
          onSubmit={(event) => {
            event.preventDefault();
            submit();
          }}
        >
          <Field data-invalid={redeem.isError} className="max-w-sm">
            <FieldLabel htmlFor="redeem-promo-code">
              {t("organization:credits.redeem.label")}
            </FieldLabel>
            <div className="flex items-center gap-2">
              <Input
                id="redeem-promo-code"
                autoComplete="off"
                className="uppercase"
                placeholder={t("organization:credits.redeem.placeholder")}
                value={code}
                onChange={(event) => {
                  setCode(event.target.value);
                  redeem.reset();
                }}
                aria-invalid={redeem.isError}
                disabled={redeem.isPending}
              />
              <Button
                type="submit"
                disabled={redeem.isPending || code.trim() === ""}
              >
                {redeem.isPending ? (
                  <Spinner />
                ) : (
                  t("organization:credits.redeem.submit")
                )}
              </Button>
            </div>
            {redeem.error && <FieldError>{redeem.error.message}</FieldError>}
          </Field>
        </form>
      </CardContent>
    </Card>
  );
};
