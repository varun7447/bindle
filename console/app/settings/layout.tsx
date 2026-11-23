import { SettingsNav } from '@/components/SettingsNav';
import { GearIcon } from '@/components/Icons';
import { settle } from '@/components/ui';
import { api } from '@/lib/api';

export default async function SettingsLayout({ children }: { children: React.ReactNode }) {
  const features = await settle(api.features());
  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="title"><span className="title-icon" aria-hidden="true"><GearIcon /></span>Settings</h1>
          <div className="sub">Connect agents, manage ingest keys, and control what the SDK sends.</div>
        </div>
      </div>
      <SettingsNav features={features.ok ? features.data.features : []} />
      {children}
    </>
  );
}
