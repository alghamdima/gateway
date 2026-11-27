"use client";

import { useTranslation } from "@workspace/i18n";
import { Button } from "@workspace/ui-web/button";
import { Icons } from "@workspace/ui-web/icons";
import { Input } from "@workspace/ui-web/input";
import {
  Modal,
  ModalBody,
  ModalClose,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
} from "@workspace/ui-web/modal";
import { Spinner } from "@workspace/ui-web/spinner";

import { useCopy } from "~/modules/common/hooks/use-copy";

/** What a confirmation is about: disabling (reversible) or deleting (not). */
export type KeyAction = "disable" | "delete";

interface ConfirmKeyDialogProps {
  readonly action: KeyAction | null;
  readonly name: string;
  readonly isPending: boolean;
  readonly onConfirm: () => void;
  readonly onClose: () => void;
}

/**
 * The confirmation before disabling or deleting a key (KEYS-04), naming it.
 * Disabling can be undone ("You can re-enable it later"); deleting cannot.
 */
export const ConfirmKeyDialog = ({
  action,
  name,
  isPending,
  onConfirm,
  onClose,
}: ConfirmKeyDialogProps) => {
  const { t } = useTranslation(["common", "organization"]);

  return (
    <Modal
      open={action !== null}
      drawer={{ onOpenChange: (open) => !open && onClose() }}
      dialog={{ onOpenChange: (open) => !open && onClose() }}
    >
      <ModalContent>
        {action && (
          <>
            <ModalHeader>
              <ModalTitle>{t(`organization:keys.${action}.title`)}</ModalTitle>
              <ModalDescription>
                {t(`organization:keys.${action}.description`, { name })}
              </ModalDescription>
            </ModalHeader>
            <ModalFooter>
              <ModalClose
                render={
                  <Button variant="outline" type="button">
                    {t("cancel")}
                  </Button>
                }
              />
              <Button
                variant="destructive"
                disabled={isPending}
                onClick={onConfirm}
              >
                {isPending ? <Spinner /> : t(`organization:keys.${action}.cta`)}
              </Button>
            </ModalFooter>
          </>
        )}
      </ModalContent>
    </Modal>
  );
};

/** The new key, shown once: it is not stored anywhere it could be shown again. */
export const SecretModal = ({
  secret,
  onClose,
}: {
  readonly secret: string | null;
  readonly onClose: () => void;
}) => {
  const { t } = useTranslation(["common", "organization"]);
  const { copied, copy } = useCopy();

  return (
    <Modal
      open={secret !== null}
      drawer={{ onOpenChange: (open) => !open && onClose() }}
      dialog={{ onOpenChange: (open) => !open && onClose() }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>{t("organization:keys.secret.title")}</ModalTitle>
          <ModalDescription>
            {t("organization:keys.secret.description")}
          </ModalDescription>
        </ModalHeader>
        <ModalBody>
          <div className="flex items-center gap-2">
            <Input
              readOnly
              value={secret ?? ""}
              className="font-mono"
              aria-label={t("organization:keys.list.key")}
              onFocus={(event) => event.target.select()}
            />
            <Button
              variant="outline"
              size="icon"
              aria-label={t("organization:keys.secret.copy")}
              onClick={() => secret && void copy(secret)}
            >
              {copied ? (
                <Icons.Check className="size-4" />
              ) : (
                <Icons.Copy className="size-4" />
              )}
            </Button>
          </div>
        </ModalBody>
        <ModalFooter>
          <Button onClick={onClose}>
            {t("organization:keys.secret.done")}
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};
