import { ApiErrorPanel, settle } from '@/components/ui';
import { ApiError, api } from '@/lib/api';
import { one } from '@/lib/ids';
import { requireSession } from '@/lib/auth';
import { PricesForm } from './PricesForm';

export const dynamic = 'force-dynamic';

export default async function PricesPage({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  await requireSession('/settings/prices');
  const region = (one((await searchParams).aws_region) ?? '').trim().toLowerCase();
  const [me, prices, models, awsRes] = await Promise.all([
    settle(api.me()),
    settle(api.prices()),
    settle(api.models('7d')),
    region ? settle(api.awsPrices(region)) : Promise.resolve(undefined),
  ]);
  const aws = !region || !awsRes ? undefined : awsRes.ok ? awsRes.data : { region, error: awsRes.error instanceof ApiError && awsRes.error.status === 400 ? awsRes.error.message : 'Could not read the AWS price list. Try again later.' };
  const canEdit = me.ok && me.data.can_manage_keys;
  return (
    <>
      <div className="section-intro">
        <div className="sub">
          Enter what you actually pay: a negotiated discount, batch pricing, or a model the list doesn&apos;t cover (Bedrock, Vertex, Azure,
          self-hosted). Workspace prices replace list prices for runs that arrive after you save; costs already recorded don&apos;t
          change. Leave cached input empty if you don&apos;t know it: calls that used cached tokens are then shown as unpriced, never
          guessed.
        </div>
      </div>
      {!prices.ok ? (
        <ApiErrorPanel error={prices.error} />
      ) : (
        <>
          {!canEdit ? (
            <div className="notice" role="note" style={{ marginBottom: 16 }}>
              <span aria-hidden="true">ⓘ</span>
              <div>Only owners and admins can change model prices.</div>
            </div>
          ) : null}
          <form id="aws-region" method="get" action="/settings/prices" />
          <PricesForm data={prices.data} models={models.ok ? models.data.models : []} canEdit={canEdit} aws={aws} />
        </>
      )}
    </>
  );
}
