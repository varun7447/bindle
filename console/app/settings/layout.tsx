import { SettingsNav } from '@/components/SettingsNav';
import { GearIcon } from '@/components/Icons';

export default function SettingsLayout({ children }: { children: React.ReactNode }) {
  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="title"><span className="title-icon" aria-hidden="true"><GearIcon /></span>Settings</h1>
          <div className="sub">Connect agents, manage ingest keys, and control what the SDK sends.</div>
        </div>
      </div>
      <SettingsNav />
      {children}
    </>
  );
}
