import { useGlobalSearchParams } from "expo-router";

import { LoginFlow } from "~/modules/auth/login";

import type { Path } from "~/config/paths";

const LoginPage = () => {
  const { redirectTo, invitationId, email } = useGlobalSearchParams<{
    redirectTo?: Path;
    invitationId?: string;
    email?: string;
  }>();

  return (
    <LoginFlow
      redirectTo={redirectTo}
      invitationId={invitationId}
      email={email}
    />
  );
};

export default LoginPage;
