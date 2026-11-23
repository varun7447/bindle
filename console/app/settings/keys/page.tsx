import { ApiErrorPanel, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { formatRelative, formatTime } from '@/lib/format';
import { keyStatus } from '@/lib/keys';
import { CreateKeyForm, RevokeKeyForm, RotateKeyForm } from './KeyForms';
import { MgmtKeys } from './MgmtKeys';

export const dynamic = 'force-dynamic';

const STATUS_LABEL = {
  active: { cls: 'badge badge-ok', icon: '✓', text: 'Active' },
  rotating: { cls: 'badge badge-warn', icon: '↻', text: 'Rotating' },
  expired: { cls: 'badge', icon: '–', text: 'Expired' },
  revoked: { cls: 'badge badge-failed', icon: '✕', text: 'Revoked' },
} as const;

export default async function KeysPage() {
  await requireSession('/settings/keys');
  const [me, keys, features] = await Promise.all([settle(api.me()), settle(api.keys()), settle(api.features())]);
  const passportsOn = features.ok && features.data.features.includes('passports');
  const now = Date.now();
  const canManage = me.ok && me.data.can_manage_keys;

  return (
    <>
      <div className="section-intro">
          <div className="sub">
            Agents send traces with these keys. Bindle stores only a hash: a key is shown once, when you create or rotate it.
          </div>
      </div>
      {!keys.ok ? (
        <ApiErrorPanel error={keys.error} />
      ) : (
        <div className="stack">
          {canManage ? (
            <section className="panel" aria-label="Create a key">
              <CreateKeyForm />
            </section>
          ) : (
            <div className="notice" role="note">
              <span aria-hidden="true">ⓘ</span>
              <div>Only owners and admins can create, rotate or revoke keys.</div>
            </div>
          )}
          <section className="panel" aria-label="Keys">
            {keys.data.keys.length === 0 ? (
              <div className="empty">No keys yet. Create one to connect your first agent.</div>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Key</th>
                    <th>Status</th>
                    <th>Created</th>
                    <th>Last used</th>
                    {canManage ? <th>Actions</th> : null}
                  </tr>
                </thead>
                <tbody>
                  {keys.data.keys.map((k) => {
                    const status = keyStatus(k, now);
                    const label = STATUS_LABEL[status];
                    return (
                      <tr key={k.id}>
                        <td className="truncate">{k.name}</td>
                        <td className="mono">{k.display_prefix}…</td>
                        <td>
                          <span className={label.cls}>
                            <span aria-hidden="true">{label.icon}</span> {label.text}
                          </span>
                          {status === 'rotating' && k.expires_at ? (
                            <div className="sub" title={formatTime(k.expires_at)}>
                              stops at {formatTime(k.expires_at)}
                            </div>
                          ) : null}
                        </td>
                        <td title={formatTime(k.created_at)}>{formatRelative(k.created_at, now)}</td>
                        <td>{k.last_used_at ? formatRelative(k.last_used_at, now) : <span className="sub">Never</span>}</td>
                        {canManage ? (
                          <td>
                            {status === 'active' ? (
                              <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                                <RotateKeyForm id={k.id} />
                                <RevokeKeyForm id={k.id} name={k.name} />
                              </div>
                            ) : status === 'rotating' ? (
                              <RevokeKeyForm id={k.id} name={k.name} />
                            ) : null}
                          </td>
                        ) : null}
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
          </section>
          {passportsOn ? <MgmtKeys canManage={canManage} /> : null}
        </div>
      )}
    </>
  );
}
