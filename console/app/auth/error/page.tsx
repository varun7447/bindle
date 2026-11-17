import Link from 'next/link';
import { one } from '@/lib/ids';

// Fixed messages only: the URL picks one by code, so a crafted link can't
// put arbitrary text on a Bindle page.
const MESSAGES: Record<string, string> = {
  cancelled: 'Sign-in was cancelled or failed.',
  mismatch: 'Your sign-in expired or did not match. Please try again.',
  exchange: 'We could not complete sign-in. Please try again.',
};

export default async function AuthError({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const message = MESSAGES[one((await searchParams).reason) ?? ''] ?? 'Sign-in failed.';
  return (
    <div className="panel" style={{ maxWidth: 520 }}>
      <h1>Couldn&apos;t sign you in</h1>
      <p className="sub">{message}</p>
      <Link className="btn btn-primary" href="/auth/login">
        Try again
      </Link>
    </div>
  );
}
