import Link from 'next/link';
import { notFound } from 'next/navigation';
import { BotIcon } from '@/components/Icons';
import { ApiErrorPanel, Help, settle } from '@/components/ui';
import { ApiError, api } from '@/lib/api';
import { requireSession } from '@/lib/auth';
import { formatRelative } from '@/lib/format';
import { one } from '@/lib/ids';
import { STATUS_LABEL, checkOf, isFlagged, learningOn, nextStep, scanReason, targetHref } from '@/lib/learning';
import type { Scan, VaultNote } from '@/lib/learning';
import { reviewAction, saveNoteAction, skillAction } from '../actions';

export const dynamic = 'force-dynamic';

const DONE: Record<string, string> = {
  approve: 'Approved.',
  retire: 'Retired.',
  restore: 'Restored as a draft.',
  review: 'Scan cleared for this text.',
  saved: 'Saved as a new version.',
};

export default async function NotePage({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  await requireSession('/agents/learning/note');
  const sp = await searchParams;
  const features = await settle(api.features());
  if (!features.ok || !learningOn(features.data.features)) notFound();
  const agent = one(sp.agent) ?? '';
  const path = one(sp.path) ?? '';
  const res = await settle(api.vaultNote(agent, path));
  const isNew = !res.ok && res.error instanceof ApiError && res.error.status === 404;
  if (!res.ok && !isNew) return <ApiErrorPanel error={res.error} />;
  const note: VaultNote | null = res.ok ? res.data : null;
  const [vault, reviewers] = await Promise.all([settle(api.vault(agent)), settle(api.securityReviewers())]);
  const owner = vault.ok ? vault.data.owner : '';
  const hasPassport = vault.ok ? vault.data.passport : false;
  const scan = note ? (note.scan as Scan) : undefined;
  const check = note ? checkOf(note) : undefined;
  const now = Date.now();
  const error = one(sp.error);
  const done = one(sp.done);
  const hidden = (sha: string) => (
    <>
      <input type="hidden" name="agent" value={agent} />
      <input type="hidden" name="path" value={path} />
      <input type="hidden" name="sha256" value={sha} />
    </>
  );

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="title">
            <span className="title-icon" aria-hidden="true">
              <BotIcon />
            </span>
            <span className="mono">{path}</span>
          </h1>
          <div className="sub">
            <Link href={`/agents/learning?${new URLSearchParams({ agent }).toString()}`}>{agent}</Link> · knowledge vault
            {note ? ` · version ${note.version} · ${note.kind}` : ' · new note'}
          </div>
        </div>
      </div>

      {error ? (
        <div className="banner banner-critical" role="alert">
          <span className="banner-icon" aria-hidden="true">
            ✕
          </span>
          <div className="banner-title">{error}</div>
        </div>
      ) : done && DONE[done] ? (
        <div className="banner banner-good" role="status">
          <span className="banner-icon" aria-hidden="true">
            ✓
          </span>
          <div className="banner-title">{DONE[done]}</div>
        </div>
      ) : null}

      {note && note.kind === 'skill' ? (
        <section className="panel" style={{ marginBottom: 16 }} aria-labelledby="status-h">
          <h2 id="status-h" style={{ marginTop: 0 }}>
            {note.status ? STATUS_LABEL[note.status] : ''} · {nextStep(note, owner, hasPassport)}
          </h2>
          <ul className="story-lines">
            <li className={scan?.flagged ? 't-warning' : 't-good'}>
              <span className="dot" aria-hidden="true">
                {scan?.flagged ? '!' : '✓'}
              </span>
              {scan?.flagged
                ? `Security scan: the text ${(scan.reasons ?? []).map(scanReason).join('; ')}.${note.reviewed_sha256 === note.sha256 ? ` Cleared by ${note.reviewed_by}.` : ''}`
                : 'Security scan: no injected instructions, hidden text or secrets.'}
            </li>
            <li className={check?.ok ? 't-good' : 't-warning'}>
              <span className="dot" aria-hidden="true">
                {check?.ok ? '✓' : '!'}
              </span>
              {check?.ok
                ? `Passport check: every tool (${(check.tools ?? []).join(', ') || 'none'}) and data class it names is in the passport.`
                : `Passport check: ${check?.reason ?? 'not run'}.`}
            </li>
          </ul>
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginTop: 12 }}>
            {note.status === 'proven' ? (
              <form action={skillAction}>
                {hidden(note.sha256)}
                <input type="hidden" name="action" value="approve" />
                <button type="submit" className="btn btn-primary btn-sm">
                  Approve
                </button>
              </form>
            ) : null}
            {isFlagged(note) && note.reviewed_sha256 !== note.sha256 && reviewers.ok && reviewers.data.me ? (
              <form action={reviewAction}>
                {hidden(note.sha256)}
                <button type="submit" className="btn btn-sm">
                  Clear the scan (security reviewer)
                </button>
              </form>
            ) : null}
            {note.status !== 'retired' ? (
              <form action={skillAction}>
                {hidden(note.sha256)}
                <input type="hidden" name="action" value="retire" />
                <button type="submit" className="btn btn-sm">
                  Retire
                </button>
              </form>
            ) : (
              <form action={skillAction}>
                {hidden(note.sha256)}
                <input type="hidden" name="action" value="restore" />
                <button type="submit" className="btn btn-sm">
                  Restore as draft
                </button>
              </form>
            )}
          </div>
        </section>
      ) : null}

      <section className="panel" style={{ marginBottom: 16 }} aria-labelledby="edit-h">
        <h2 id="edit-h" style={{ marginTop: 0 }}>
          {note ? 'Text' : 'Write the note'}{' '}
          <Help>
            Saving creates a new version. Secrets and personal data are redacted before storage. Editing a skill makes it a draft again: its proof, review
            and approval apply to the text they were given for.
          </Help>
        </h2>
        <form action={saveNoteAction}>
          <input type="hidden" name="agent" value={agent} />
          <input type="hidden" name="path" value={path} />
          <input type="hidden" name="base" value={note?.sha256 ?? ''} />
          <label className="visually-hidden" htmlFor="note-content">
            Note text
          </label>
          <textarea id="note-content" name="content" rows={18} defaultValue={note?.content ?? ''} className="mono" style={{ width: '100%' }} required />
          <button type="submit" className="btn btn-sm" style={{ marginTop: 8 }}>
            Save
          </button>
        </form>
      </section>

      {note ? (
        <div className="grid-2">
          <section className="panel" aria-labelledby="links-h">
            <h2 id="links-h" style={{ marginTop: 0 }}>
              Links
            </h2>
            {(note.links ?? []).length === 0 && (note.backlinks ?? []).length === 0 ? <div className="empty">No [[links]] to or from this note.</div> : null}
            {(note.links ?? []).length > 0 ? (
              <>
                <div className="sub">Links to</div>
                <ul>
                  {note.links!.map((l) => (
                    <li key={l}>
                      <Link href={targetHref(l, agent)} className="mono">
                        {l}
                      </Link>
                    </li>
                  ))}
                </ul>
              </>
            ) : null}
            {(note.backlinks ?? []).length > 0 ? (
              <>
                <div className="sub">Linked from</div>
                <ul>
                  {note.backlinks!.map((l) => (
                    <li key={l}>
                      <Link href={targetHref(l, agent)} className="mono">
                        {l}
                      </Link>
                    </li>
                  ))}
                </ul>
              </>
            ) : null}
          </section>
          <section className="panel panel-flush" aria-labelledby="history-h">
            <div className="section-title">
              <h2 id="history-h">History</h2>
              <span className="sub">Newest first · every version is kept</span>
            </div>
            <table>
              <thead>
                <tr>
                  <th className="num" style={{ paddingLeft: 18 }}>
                    #
                  </th>
                  <th>What</th>
                  <th>Who</th>
                  <th style={{ paddingRight: 18 }}>When</th>
                </tr>
              </thead>
              <tbody>
                {(note.versions ?? []).map((v) => (
                  <tr key={v.version}>
                    <td className="num" style={{ paddingLeft: 18 }}>
                      {v.version}
                    </td>
                    <td>
                      {v.action}
                      {v.status ? ` → ${v.status}` : ''}
                    </td>
                    <td className="mono">{v.edited_by}</td>
                    <td style={{ paddingRight: 18 }}>{formatRelative(v.edited_at, now)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </section>
        </div>
      ) : null}
    </>
  );
}
