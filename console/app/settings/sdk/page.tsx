import { ApiErrorPanel, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { formatTime } from '@/lib/format';
import { SettingsForm } from './SettingsForm';

export const dynamic = 'force-dynamic';

export default async function SettingsPage() {
  await requireSession('/settings/sdk');
  const [me, config] = await Promise.all([settle(api.me()), settle(api.config())]);
  const canEdit = me.ok && me.data.can_manage_keys;
  return (
    <>
      <div className="section-intro">
          <div className="sub">
            Change how your agents send traces without redeploying. These settings are a ceiling: code can only be stricter.
            {config.ok && config.data.updated_at ? ` Last changed ${formatTime(config.data.updated_at)}.` : ''}
          </div>
      </div>
      {!config.ok ? (
        <ApiErrorPanel error={config.error} />
      ) : (
        <>
          {!canEdit ? (
            <div className="notice" role="note" style={{ marginBottom: 16 }}>
              <span aria-hidden="true">ⓘ</span>
              <div>Only owners and admins can change these settings.</div>
            </div>
          ) : null}
          <SettingsForm config={config.data} canEdit={canEdit} />
        </>
      )}
    </>
  );
}
