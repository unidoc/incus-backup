// safeNext keeps a post-sign-in destination on this site. It mirrors auth.SafeNext on the server
// (internal/server/auth/oidc.go): a path, never another host. Browsers delete tab, CR and LF before
// parsing a URL, so "/\t/evil.example" would become "//evil.example"; every control character is refused.
export function safeNext(next: unknown): string {
  if (typeof next !== 'string' || !next.startsWith('/')) return '/'
  if (next.startsWith('//') || next.startsWith('/\\') || next.startsWith('/api/')) return '/'
  for (let i = 0; i < next.length; i++) {
    const c = next.charCodeAt(i)
    if (c < 0x20 || c === 0x7f) return '/'
  }
  return next
}
