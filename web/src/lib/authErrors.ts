// Plain-English messages for the security answers of the sign-in, setup and profile routes.
//
//  * 403 on POST /session or /setup: the server refuses a cross-site request (a foreign Origin, or a
//    body that is not JSON): the page is not running on the server's own address, or a proxy rewrites
//    the Origin/Host headers.
//  * 429 on a password re-check (change password, turn off 2FA): too many wrong passwords locked
//    password checks for the account for a few minutes (1, 2, 4, 8, then 15 minutes); new sign-ins
//    with the account are refused meanwhile, this session stays valid.
import { ApiError } from '@/api/client'

/** The hint for a refused cross-site sign-in or setup, or null when e is something else. */
export function crossSiteHint(e: unknown): string | null {
  if (!(e instanceof ApiError) || e.status !== 403) return null
  if (!/cross-site|origin/i.test(e.message)) return null
  return 'The server refused this as a cross-site request. Open the console at the server\'s own address (the one in your bookmark or the server log), not through another site or a proxy that rewrites the Origin or Host header, then try again.'
}

/** The message for a failed sign-in (step: which form was submitted). */
export function signInError(e: unknown, step: 'password' | 'code' = 'password'): string {
  const xs = crossSiteHint(e)
  if (xs) return xs
  if (e instanceof ApiError) {
    if (e.status === 401) return step === 'code' ? 'That code is not valid. Codes change every 30 seconds; a used code cannot be reused.' : 'The name or password is wrong.'
    if (e.status === 429) return 'Too many failed attempts. Sign-in for this account is locked for a few minutes (or this address tried too often). Wait, then try again.'
    return e.message
  }
  return e instanceof Error ? e.message : String(e)
}

/** The message for a failed password re-check on the profile page (change password, turn off 2FA). */
export function passwordCheckError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 429) return 'Too many wrong passwords. Password checks for your account are locked for a few minutes, and new sign-ins with it are refused meanwhile; you stay signed in here. Wait a few minutes, then try again with the right password.'
    if (e.status === 401) return 'Your current password is wrong. After five wrong passwords, checks lock for a few minutes.'
    if (e.status === 403 && /csrf/i.test(e.message)) return 'The server refused the request (security token mismatch). Reload the page and try again.'
    return e.message
  }
  return e instanceof Error ? e.message : String(e)
}
