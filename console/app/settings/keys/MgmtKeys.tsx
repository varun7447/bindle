import { ApiErrorPanel, settle } from '@/components/ui';
import { api } from '@/lib/api';
import { formatRelative, formatTime } from '@/lib/format';
import { SCOPE_LABEL } from '@/lib/passports';
import { CreateMgmtKeyForm, RevokeMgmtKeyForm } from './KeyForms';

/** Management keys: for the CLI and CI (passports), never for agents. */
export async function MgmtKeys({ canManage }: { canManage: boolean }) {
  const res = await settle(api.mgmtKeys());
  const now = Date.now();
  return (
    <section className="panel" aria-labelledby="mgmt-h" style={{ marginTop: 16 }}>
      <div className="section-title" style={{ padding: 0 }}>
        <h2 id="mgmt-h">Management keys</h2>
        <span className="sub">For the CLI and CI, e.g. pushing passports. Agents can&apos;t use them, and ingest keys can&apos;t do this.</span>
      </div>
      {!res.ok ? (
        <ApiErrorPanel error={res.error} />
      ) : (
        <div className="stack" style={{ marginTop: 12 }}>
          {canManage ? <CreateMgmtKeyForm scopes={res.data.scopes} /> : null}
          {res.data.keys.length === 0 ? (
            <div className="empty">No management keys.</div>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Key</th>
                  <th>May</th>
                  <th>Created</th>
                  <th>Last used</th>
                  {canManage ? <th>Actions</th> : null}
                </tr>
              </thead>
              <tbody>
                {res.data.keys.map((k) => (
                  <tr key={k.id}>
                    <td className="truncate">{k.name}</td>
                    <td className="mono">{k.display_prefix}…</td>
                    <td>{k.scopes.map((s) => SCOPE_LABEL[s] ?? s).join(', ')}</td>
                    <td title={formatTime(k.created_at)}>{formatRelative(k.created_at, now)}</td>
                    <td>
                      {k.revoked_at ? (
                        <span className="badge badge-failed">
                          <span aria-hidden="true">✕</span> Revoked
                        </span>
                      ) : k.last_used_at ? (
                        formatRelative(k.last_used_at, now)
                      ) : (
                        <span className="sub">Never</span>
                      )}
                    </td>
                    {canManage ? <td>{k.revoked_at ? null : <RevokeMgmtKeyForm id={k.id} name={k.name} />}</td> : null}
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </section>
  );
}
