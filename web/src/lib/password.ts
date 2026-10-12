// Password guidance. The server's only rule is length >= 12 characters (auth.MinPassword, counted in
// Unicode characters); everything else here is advice, never a blocker.
export const MIN_PASSWORD = 12

export interface Strength {
  /** 0 too short (server refuses) .. 4 strong. */
  score: 0 | 1 | 2 | 3 | 4
  label: string
  acceptable: boolean
  hints: string[]
}

const COMMON = ['password', 'passwort', 'qwerty', 'letmein', 'welcome', 'admin', 'incus', 'backup', '123456', 'abc123', 'iloveyou', 'monkey', 'dragon']

export function passwordStrength(pw: string, context: string[] = []): Strength {
  const len = [...pw].length
  const hints: string[] = []
  if (len < MIN_PASSWORD) {
    hints.push(`Use at least ${MIN_PASSWORD} characters (${MIN_PASSWORD - len} more).`)
    return { score: 0, label: 'Too short', acceptable: false, hints }
  }
  const classes = [/[a-z]/, /[A-Z]/, /[0-9]/, /[^A-Za-z0-9]/].filter((r) => r.test(pw)).length
  const lower = pw.toLowerCase()
  let score = 1
  if (len >= 16) score++
  if (len >= 20) score++
  if (classes >= 3 || /\s/.test(pw.trim())) score++
  const unique = new Set(pw).size
  if (unique <= Math.max(3, len / 4)) { score = Math.min(score, 1); hints.push('Avoid repeating the same few characters.') }
  if (/(.)\1{3,}/.test(pw)) { score = Math.min(score, 2); hints.push('Avoid runs of one character.') }
  if (/(0123|1234|2345|3456|4567|5678|6789|abcd|bcde|qwer|asdf)/i.test(pw)) { score = Math.min(score, 2); hints.push('Avoid sequences like 1234 or qwer.') }
  if (COMMON.some((w) => lower.includes(w))) { score = Math.min(score, 2); hints.push('Avoid common words such as “password” or the product name.') }
  for (const c of context) {
    if (c && c.length >= 3 && lower.includes(c.toLowerCase())) { score = Math.min(score, 1); hints.push('Do not include your user name.'); break }
  }
  if (score < 3 && len < 16) hints.push('A longer passphrase of several unrelated words is easier to remember and stronger.')
  const s = Math.max(1, Math.min(4, score)) as 1 | 2 | 3 | 4
  return { score: s, label: ['Too short', 'Weak', 'Fair', 'Good', 'Strong'][s], acceptable: true, hints }
}
