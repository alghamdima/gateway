"use client";

import { useMutation } from "@tanstack/react-query";
import { useRouter } from "next/navigation";
import { toast } from "sonner";

import { useTranslation } from "@workspace/i18n";
import { Button } from "@workspace/ui-web/button";
import { Icons } from "@workspace/ui-web/icons";
import {
  Modal,
  ModalClose,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
  ModalTrigger,
} from "@workspace/ui-web/modal";

import { pathsConfig } from "~/config/paths";
import { authClient } from "~/lib/auth/client";
import {
  SettingsCard,
  SettingsCardHeader,
  SettingsCardTitle,
  SettingsCardDescription,
  SettingsCardFooter,
} from "~/modules/common/layout/dashboard/settings-card";

import { user } from "../../lib/api";

import type { User } from "@workspace/auth";

interface DeleteAccountProps {
  readonly user: User;
}

export const DeleteAccount = ({ user }: DeleteAccountProps) => {
  const { t } = useTranslation("auth");

  return (
    <SettingsCard variant="destructive">
      <SettingsCardHeader>
        <SettingsCardTitle>{t("account.delete.title")}</SettingsCardTitle>
        <SettingsCardDescription>
          {t("account.delete.description")}
        </SettingsCardDescription>
      </SettingsCardHeader>

      <SettingsCardFooter>
        <ConfirmModal isAnonymous={user.isAnonymous ?? false}>
          <Button size="sm" className="ml-auto" variant="destructive">
            {t("account.delete.cta")}
          </Button>
        </ConfirmModal>
      </SettingsCardFooter>
    </SettingsCard>
  );
};

const ConfirmModal = ({
  children,
  isAnonymous,
}: {
  children: React.ReactNode;
  isAnonymous: boolean;
}) => {
  const session = authClient.useSession();
  const router = useRouter();
  const { t } = useTranslation(["common", "auth"]);

  const deleteAccount = useMutation({
    ...user.mutations.delete.user,
    onSuccess: () => {
      toast.success(t("account.delete.confirmation.success"));
    },
  });

  const deleteAnonymousAccount = useMutation({
    ...user.mutations.delete.anonymous,
    onSuccess: async () => {
      await session.refetch();
      router.replace(pathsConfig.index);
      toast.success(t("account.delete.anonymous.confirmation.success"));
    },
  });

  const disclaimer = isAnonymous
    ? t("account.delete.anonymous.disclaimer")
    : t("account.delete.disclaimer");

  const renderCta = () =>
    isAnonymous ? (
      <Button
        onClick={() => deleteAnonymousAccount.mutate(undefined)}
        variant="destructive"
        disabled={deleteAnonymousAccount.isPending}
      >
        {deleteAnonymousAccount.isPending ? (
          <Icons.Loader2 className="animate-spin" />
        ) : (
          t("account.delete.cta")
        )}
      </Button>
    ) : (
      <Button
        onClick={() =>
          deleteAccount.mutate({
            callbackURL: pathsConfig.index,
          })
        }
        variant="destructive"
        disabled={deleteAccount.isPending}
      >
        {deleteAccount.isPending ? (
          <Icons.Loader2 className="animate-spin" />
        ) : (
          t("account.delete.confirmation.cta")
        )}
      </Button>
    );

  return (
    <Modal>
      <ModalTrigger asChild>{children}</ModalTrigger>
      <ModalContent>
        <ModalHeader>
          <ModalTitle>{t("account.delete.title")}</ModalTitle>
          <ModalDescription className="whitespace-pre-line">
            {disclaimer}
          </ModalDescription>
        </ModalHeader>
        <ModalFooter>
          <ModalClose asChild>
            <Button variant="outline">{t("cancel")}</Button>
          </ModalClose>
          {renderCta()}
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};
