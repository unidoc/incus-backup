// The browser side of passkeys and security keys: the server's options (JSON, binary fields as
// base64url, exactly what go-webauthn sends) to navigator.credentials.create()/get() and the answer back
// to JSON. No library: the Web Authentication API plus a few lines of conversion. Errors become plain
// English (cancelled, wrong address, not supported, insecure page).

export function b64urlToBytes(s: string): Uint8Array<ArrayBuffer> {
  const b64 = s.replace(/-/g, '+').replace(/_/g, '/') + '==='.slice((s.length + 3) % 4)
  const bin = atob(b64)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

export function bytesToB64url(b: ArrayBuffer | ArrayBufferView): string {
  const u = b instanceof ArrayBuffer ? new Uint8Array(b) : new Uint8Array(b.buffer, b.byteOffset, b.byteLength)
  let bin = ''
  for (let i = 0; i < u.length; i++) bin += String.fromCharCode(u[i])
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

interface Descriptor { type: 'public-key'; id: string; transports?: string[] }
/** The server's options, as JSON (go-webauthn protocol.CredentialCreation / CredentialAssertion). */
export interface ServerOptions { publicKey: Record<string, unknown> & { challenge: string } ; mediation?: string }

function descriptors(l: unknown): PublicKeyCredentialDescriptor[] | undefined {
  if (!Array.isArray(l)) return undefined
  return (l as Descriptor[]).map((d) => ({ type: 'public-key', id: b64urlToBytes(d.id), transports: d.transports as AuthenticatorTransport[] | undefined }))
}

/** Options for navigator.credentials.create() from the server's JSON. */
export function creationOptions(o: ServerOptions): PublicKeyCredentialCreationOptions {
  const pk = o.publicKey as Record<string, unknown>
  const user = pk.user as { id: string; name: string; displayName: string }
  return {
    ...(pk as object),
    challenge: b64urlToBytes(pk.challenge as string),
    user: { ...user, id: b64urlToBytes(user.id) },
    excludeCredentials: descriptors(pk.excludeCredentials),
  } as unknown as PublicKeyCredentialCreationOptions
}

/** Options for navigator.credentials.get() from the server's JSON. */
export function requestOptions(o: ServerOptions): PublicKeyCredentialRequestOptions {
  const pk = o.publicKey as Record<string, unknown>
  return {
    ...(pk as object),
    challenge: b64urlToBytes(pk.challenge as string),
    allowCredentials: descriptors(pk.allowCredentials),
  } as unknown as PublicKeyCredentialRequestOptions
}

/** The JSON form of a PublicKeyCredential that go-webauthn parses (base64url binary fields). */
export function credentialJSON(c: PublicKeyCredential): Record<string, unknown> {
  const r = c.response as AuthenticatorAttestationResponse & AuthenticatorAssertionResponse
  const response: Record<string, unknown> = { clientDataJSON: bytesToB64url(r.clientDataJSON) }
  if ('attestationObject' in r && r.attestationObject) {
    response.attestationObject = bytesToB64url(r.attestationObject)
    if (typeof r.getTransports === 'function') response.transports = r.getTransports()
  } else {
    response.authenticatorData = bytesToB64url(r.authenticatorData)
    response.signature = bytesToB64url(r.signature)
    if (r.userHandle) response.userHandle = bytesToB64url(r.userHandle)
  }
  return {
    id: c.id,
    rawId: bytesToB64url(c.rawId),
    type: c.type,
    authenticatorAttachment: c.authenticatorAttachment ?? undefined,
    clientExtensionResults: c.getClientExtensionResults ? c.getClientExtensionResults() : {},
    response,
  }
}

export class WebAuthnUnavailable extends Error {}

/** Why passkeys cannot work on this page ('' when they can). */
export function unavailableReason(): string {
  if (typeof window === 'undefined') return 'Passkeys need a browser.'
  if (!window.isSecureContext) return `Passkeys and security keys need a secure page: open the console over HTTPS (or on localhost). This page is ${location.origin}.`
  if (!('PublicKeyCredential' in window) || !navigator.credentials) return 'This browser does not support passkeys or security keys. Use a current Chrome, Edge, Firefox or Safari.'
  return ''
}

function ensureAvailable() {
  const r = unavailableReason()
  if (r) throw new WebAuthnUnavailable(r)
}

/** Ask the authenticator to create a credential; returns the JSON to send to the server. */
export async function createCredential(o: ServerOptions, signal?: AbortSignal): Promise<Record<string, unknown>> {
  ensureAvailable()
  const c = (await navigator.credentials.create({ publicKey: creationOptions(o), signal })) as PublicKeyCredential | null
  if (!c) throw new WebAuthnUnavailable('The browser returned no credential.')
  return credentialJSON(c)
}

/** Ask for an assertion; returns the JSON to send to the server. */
export async function getAssertion(o: ServerOptions, signal?: AbortSignal): Promise<Record<string, unknown>> {
  ensureAvailable()
  const c = (await navigator.credentials.get({ publicKey: requestOptions(o), signal })) as PublicKeyCredential | null
  if (!c) throw new WebAuthnUnavailable('The browser returned no credential.')
  return credentialJSON(c)
}

/** Plain English for what navigator.credentials threw (the server's own errors are ApiErrors). */
export function webauthnError(e: unknown, what: 'register' | 'sign-in' = 'sign-in'): string {
  if (e instanceof WebAuthnUnavailable) return e.message
  const name = (e as { name?: string } | null)?.name ?? ''
  switch (name) {
    case 'NotAllowedError':
      return what === 'register'
        ? 'The security key prompt was cancelled or timed out. Try again and confirm on your key or device.'
        : 'The passkey prompt was cancelled or timed out, or no passkey for this site is on this device. Try again, or sign in another way.'
    case 'InvalidStateError':
      return 'This security key is already registered for your account. Use another key, or rename the existing one.'
    case 'SecurityError':
      return `The browser refused this site for passkeys: the console address (${typeof location !== 'undefined' ? location.origin : 'this page'}) does not match the passkey domain the server is set up for. Open the console at its configured address (Settings → Authentication).`
    case 'NotSupportedError':
      return 'This key or device does not support what the server asks for (a passkey with user verification). Try another key or a phone.'
    case 'AbortError':
      return 'Cancelled.'
  }
  return e instanceof Error ? e.message : String(e)
}
