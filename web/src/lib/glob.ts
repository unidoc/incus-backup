// The glob of the notification engine (internal/server/notify/settings.go glob()), for previews in
// the browser ("this silence matches 3 open alerts"). Anchored; `*` matches anything including `/`,
// `?` one character; "" and "*" match everything. The server decides; this only previews.
const cache = new Map<string, RegExp | null>()

export function globMatch(pattern: string, value: string): boolean {
  if (pattern === '' || pattern === '*') return true
  let re = cache.get(pattern)
  if (re === undefined) {
    let src = '^'
    for (const ch of pattern) {
      if (ch === '*') src += '.*'
      else if (ch === '?') src += '.'
      else src += ch.replace(/[.*+?^${}()|[\]\\/-]/g, '\\$&')
    }
    re = new RegExp(src + '$')
    cache.set(pattern, re)
  }
  return re!.test(value)
}

export interface Matchers { type_glob: string; key_glob: string; host_glob: string; repo_glob: string; object_glob: string }
export interface Matchable { type: string; key: string; host: string; repository: string; object: string }

/** Silence.matches of the engine. */
export function silenceMatches(s: Matchers, a: Matchable): boolean {
  return globMatch(s.type_glob, a.type) && globMatch(s.key_glob, a.key) && globMatch(s.host_glob, a.host) && globMatch(s.repo_glob, a.repository) && globMatch(s.object_glob, a.object)
}

/** At least one matcher set (the server refuses a silence that matches everything by accident). */
export function hasMatcher(s: Matchers): boolean {
  return !!(s.type_glob || s.key_glob || s.host_glob || s.repo_glob || s.object_glob)
}
