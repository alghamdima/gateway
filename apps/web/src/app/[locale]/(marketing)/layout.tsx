import { getSession } from "~/lib/auth/server";
import { Footer } from "~/modules/marketing/layout/footer/footer";
import { Header } from "~/modules/marketing/layout/header/header";
import { getSiteSettings } from "~/modules/marketing/site";

export default async function MainLayout(props: { children: React.ReactNode }) {
  const [session, site] = await Promise.all([getSession(), getSiteSettings()]);

  return (
    <>
      <Header initialSession={session} site={site} />
      <main className="w-full">{props.children}</main>
      <Footer />
    </>
  );
}
