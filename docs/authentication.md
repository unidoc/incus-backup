# Authentication: passwords, second factors, passkeys and single sign-on

People sign in to the web console and the management API (`/api/v1`) in one of these ways:

| method | what proves the person | second factor |
|---|---|---|
| password | user name + password (argon2id) | TOTP code, a recovery code, or a security key, when the user has one |
| passkey | a discoverable WebAuthn credential with user verification (PIN, fingerprint, face) | built in: possession of the key plus user verification |
| single sign-on | an OpenID Connect identity provider (Keycloak, Authentik, Google, ...) | the identity provider's business |

Backup hosts never use any of this: they authenticate with mutual TLS (`docs/certificates.md`).
API tokens (`Authorization: Bearer ibt_…`) belong to a user and are not affected by the sign-in policy
below. See "API tokens" below for how they are created and how long they live.

**One session model.** Every method ends in the same place: `auth.Store.openSession` creates the row in
`user_sessions`, the same `ib_session` cookie (HttpOnly, Secure, SameSite=Strict) and the same CSRF token
as a password sign-in, with the same lifetime (30 minutes idle, 12 hours at most). Roles, repository scopes
(`docs/permissions.md`), the audit log and the lock-out after failed attempts are the same for every
method. The lock-out looks the same for every name: an unknown name or a single sign-on account answers 401 five times and then 429 like a real account (counted in memory, so a restart forgets it). The in-memory part is bounded: names are hashed, names over 256 bytes are refused as an ordinary 401, an entry expires 24 hours after its last failure, and when 10000 entries are live the one expiring soonest is evicted; an attacker who sends 10000 other fresh names can therefore reset a name's count again, at the price of a password-hash round each. Failures are counted per account, so anyone who keeps guessing a real user's name locks that user out (1-15 minutes at a time); that is the existing behaviour and the audit log shows the source address. `GET /session` says how a session was opened (`method`: `password`, `passkey` or `oidc`).

Code: `internal/server/auth/{auth,webauthn,oidc,policy}.go`, routes in
`internal/server/mgmt/routes_authn.go`, schema in section 14 of
`migrations/20261008000000_initial_schema.sql`, web pages `web/src/views/auth/Login.vue`,
`web/src/views/auth/Enrol2FA.vue`, `web/src/views/settings/Authentication.vue`,
`web/src/components/authn/*`.

### What counts as activity (the 30-minute idle timeout)

A session's idle clock moves only when a person's request reaches the server (`last_seen`, written at
most once a minute). The console's open event stream (`GET /events`) checks the session every 20 seconds
so a revoked or expired session ends the stream, but that check does NOT renew it (`Store.Recheck`).
The console pauses every timed poll and every event-driven refetch while its tab is hidden
(`document.visibilityState`; the shared helpers are `web/src/lib/poll.ts` and `EventBus.onVisible`) and
catches up once when the tab becomes visible again (events that arrived meanwhile are replayed, at most 50
per subscription). Only the idle watcher's own `GET /session`, sent after the idle limit has already passed,
runs in a hidden tab. So a **visible** tab that keeps polling or refetching stays signed in (a console on a wall
screen is meant to), and a **hidden** or forgotten tab reaches the 30-minute idle timeout. The 12-hour
absolute limit applies to both.

## API tokens

- Creating one (`POST /me/tokens`) and starting or confirming two-factor enrolment (`POST /me/totp`,
  `/me/totp/confirm`) need a sign-in within the last **10 minutes** or the password again (`password`
  in the body; the answer is 400 `password_required` otherwise, a wrong password counts like a failed
  sign-in against the account, so someone holding a stolen cookie can lock the real user out for the
  usual 1-15 minutes by guessing; `DELETE /me/totp` behaves the same), the same as adding or removing a
  passkey. A user without a local password (single sign-on only) is told so (400, "single sign-on") on
  the token, TOTP and passkey routes before any password is asked for. A stolen session cookie alone therefore cannot mint
  a key that outlives the cookie.
- A token always expires: it is bounded when it is created. `ttl_hours` missing or 0 means **90 days**; the
  longest is **365 days** (more, a negative value, or a positive value that rounds to zero is refused with
  400). A scrape token (docs/metrics.md) must therefore be re-created before it expires.
- A user who signs in only through single sign-on (no local password) gets **no API tokens** (400), for
  the same reason as for passkeys and TOTP: a local key would bypass the identity provider and keep the
  last role it mapped, because the token's role is capped by `users.role`, which only the next
  single sign-on sign-in rewrites. Such users who need automation use a token of a local account.
- Tokens outlive a password change or reset (that ends sessions, not tokens): revoke them from
  Profile → API tokens, or delete the user. Revoking tokens on an administrator's password reset is
  not done automatically.

## Passkeys and security keys (WebAuthn)

Built on [github.com/go-webauthn/webauthn](https://github.com/go-webauthn/webauthn) on the server and the
browser's own Web Authentication API (`navigator.credentials`, `web/src/lib/webauthn.ts`); no other library.

### What the server needs

- **A domain name and HTTPS.** Browsers allow passkeys only on a secure page (HTTPS, or `localhost`) and
  only for a domain name, never an IP address. Open the console at its DNS name with a certificate the
  browser trusts (`docs/certificates.md`): Chrome refuses WebAuthn on a page with a certificate error,
  except in the headless test browser.
- **The relying party.** By default the console address `base_url` (Settings → Server) is the only allowed
  origin and its host the passkey domain (RP ID). Settings → Authentication → *Override the domain or
  origins* sets `webauthn_rp_id` (the host or a parent domain, e.g. `example.com`) and `webauthn_origins`
  (every `https://host[:port]` people open the console at). The server refuses an RP ID that does not
  cover the origins. **Changing the RP ID later makes every registered key stop working** (keys are bound
  to it by the authenticator).
- Without `base_url` (or origins) passkeys are off: the sign-in page does not offer them, and the routes
  answer "passkeys are not configured: set the console address". Settings → Authentication shows a
  self-check: the server's configuration, whether *this* page is an allowed origin, whether the browser
  supports passkeys on it, and whether the device has a built-in authenticator.

### Registering (Profile → Security keys & passkeys)

1. *Add a security key or passkey*, give it a name (`POST /me/webauthn/register/begin`).
   The server asks for a **discoverable** credential (resident key *preferred*) so the same key can sign in
   without a user name, user verification *preferred*, attestation *none* (no attestation is checked or
   kept), and excludes keys already registered.
2. The browser asks you to touch the key or confirm on the phone/computer; the result goes to
   `POST /me/webauthn/register/finish`, which verifies it (challenge, origin, RP ID, signature, flags).
3. Adding or removing a key needs a sign-in within the last **10 minutes**, or the password again (the
   answer then carries `password_required: true` and the console asks for it). A wrong password counts
   like a failed sign-in.

Keys can be renamed (`PATCH /me/webauthn/{id}`) and removed (`DELETE /me/webauthn/{id}`). The list shows
for each key whether it is a synced passkey (backup eligible), when it was last used, and whether it is
**blocked** (see clone detection).

Users who sign in only through single sign-on (no local password) cannot register keys, TOTP or API tokens: their
second factor is the identity provider's (and a local key would bypass the provider and keep the last
role it mapped).

### Signing in

- **Passkey** (*Sign in with a passkey*, no name typed): `POST /session/webauthn/begin` with no body
  returns options that name no user and allow any credential (the same answer for everybody, so nothing can
  be enumerated); the authenticator picks the credential; `POST /session/webauthn/finish` finds the user by
  the credential's user handle (a random 32-byte value, never the user name) and requires user
  verification.
- **Second factor after the password**: when a user with a key signs in with the password, `POST /session`
  answers 401 `{"totp_required": true, "second_factor": {"totp": …, "webauthn": true}, "ticket": "…"}`. The
  ticket (single use, 5 minutes, stored hashed) starts the assertion (`POST /session/webauthn/begin
  {"ticket": "…"}`), limited to that user's credentials; the password is not sent again. A security key
  satisfies the second factor exactly like a TOTP code; **a password alone never opens a session for a
  user who has a key**, and a TOTP-looking code cannot stand in for the key. Users with both may use either.

### Security properties

- **Ceremony state** (go-webauthn `SessionData`) lives server-side in `auth_ceremonies` for 5 minutes and
  is deleted when finish starts, whatever the outcome: a challenge can be answered once. Ceremonies are
  bound to their user (registration: the signed-in user; second factor: the user whose password was
  verified).
- **Origin and RP ID** of every answer are checked by go-webauthn against the configured values; an answer
  produced on another site (phishing) or for another RP is refused.
- **Clone detection.** The signature counter must grow. go-webauthn only *flags* a counter that does not;
  the server refuses that sign-in, marks the key `clone_warning` and refuses it from then on, also with
  higher counters. Remove it and register a new one. (Synced passkeys usually report counter 0, which is
  accepted as "no counter".)
- **Failures count**: a refused assertion for a known account counts as a failed sign-in of that account
  (the same 1-15 minute lock as wrong passwords). An unknown credential counts against nobody. All
  passkey routes are rate limited per address like the password route (20 per minute).
- **Audit**: every sign-in and failure is an `action=login` entry with `detail.method` (`password`,
  `password+totp`, `webauthn`, `passkey`, `oidc`) and the reason of a failure; registrations, renames and
  removals are audited like every state-changing request.

## Requiring a second factor (Settings → Authentication)

`require_2fa`: `none` (default), `admin` (users whose global role is admin) or `all`. A covered user who
signs in with a password and has neither TOTP nor a key gets a session that reaches only the enrolment
routes (`GET/DELETE /session`, `/me/totp`, `/me/totp/confirm`, `/me/webauthn`, register begin/finish);
everything else answers 403 `{"enrol_2fa_required": true}` and the console shows the enrolment page. The
decision is made on every request, so changing the policy or removing a factor applies at once, and the
limit lifts as soon as a factor is set up.

- Single sign-on sessions are never limited (the identity provider owns the second factor); passkey
  sessions are a second factor themselves.
- While the policy covers a user, the user cannot remove their **last** factor (`DELETE /me/totp`,
  `DELETE /me/webauthn/{id}` answer 400). An administrator's reset (lost device) is always allowed; the
  user is then asked to enrol again at the next request.
- First enrolment trusts whoever holds the password at that moment (as every forced-enrolment scheme does):
  turn the policy on before passwords leak, not after.
- The policy is NOT applied to API tokens.
- The console learns that its own session became limited when it reads `GET /session` (at load, and right
  after an administrator changes the policy on Settings → Authentication). Another person whose session is
  limited by a policy change while their console is open sees "two-factor authentication is required"
  errors until they reload the page, which then opens the enrolment page.

## Single sign-on (OpenID Connect)

Built on [github.com/coreos/go-oidc/v3](https://github.com/coreos/go-oidc) and
[golang.org/x/oauth2](https://pkg.go.dev/golang.org/x/oauth2). Providers are data in PostgreSQL
(`oidc_providers`), edited in Settings → Authentication or `/api/v1/oidc/providers`; nothing is in a file.

### The flow

1. The sign-in page lists enabled providers (`GET /auth/oidc/providers`, public, names and labels only). The
   button is a plain link to `GET /auth/oidc/{name}/login?next=/path`.
2. The server creates a **state** (stored hashed), a **nonce** and a **PKCE verifier** (S256), single use,
   10 minutes (`oidc_logins`), sets a short cookie `ib_oidc` (HttpOnly, SameSite=Lax, path
   `/api/v1/auth/oidc/`) that binds the attempt to this browser, and redirects to the provider's
   authorization endpoint (Authorization Code flow).
3. The provider redirects back to `GET /auth/oidc/{name}/callback`. The server deletes the state row
   (single use), checks it belongs to this provider and this browser's cookie, exchanges the code with the
   PKCE verifier (and the client secret, when set), verifies the ID token with go-oidc (signature against the
   provider's JWKS, issuer, audience = client id, expiry) and the nonce, requires `email_verified: true` and
   an allowed e-mail domain, then finds the account (below), applies the role mappings and opens the
   session. The browser goes to `next` (a path on this site only).
4. A failure redirects to `/login?sso_error=<word>` (`state`, `denied`, `unverified`, `domain`,
   `no_account`, `exists`, `disabled`, `locked`, `provider`, `config`, `failed`); the page explains the word.
   The provider's own error text is never shown (no reflection); the audit log has the reason.

### Accounts: by subject, never by e-mail alone

A user is linked to a provider by the pair (provider, `sub`) in `user_identities`. The e-mail address is
never enough to sign in as an existing account (that would let anyone who controls an address at the
provider take over the account). An account is found:

1. by the subject already linked; else
2. by an **administrator's explicit link** (Users → … → *Single sign-on*, `POST /users/{name}/identities
   {provider, email}`): the first sign-in at that provider with that **verified** address binds the account
   to the subject, once; afterwards only the subject matches; else
3. by **automatic creation** when the provider has `auto_create` on: a new user named after
   `preferred_username` (or the address's local part), with **no local password**, created with the
   default role. If a local user of that name exists, the sign-in is refused (`exists`) rather than linked.

With automatic creation off, an unknown subject is refused (`no_account`). Disabled users cannot sign in;
an account locked after failed password attempts is refused until the lock ends.

### Roles and repository scopes from the token

Each provider has a **default role** (`none`, `readonly`, `restore`, `operator`; never admin: the server
refuses it), **group → role** mappings and **group → repository** mappings, read from the claim named in
`groups_claim` (default `groups`; a list of strings or one string):

- role = the highest of the default role and the role of every mapping whose group the user is in;
  administrator only through an explicit mapping (e.g. `backup-admins → admin`);
- when the provider has at least one repository mapping and the role is not admin, the user is restricted
  (`users.repo_scoped`) to the repositories of the matching mappings, with the highest mapped role on each;
  a user in no mapped group sees **no repository**. Without repository mappings the role applies to every
  repository.

**Both are recomputed at every sign-in** and replace what is stored, so the provider is authoritative: an
administrator's manual change to such a user lasts until their next sign-in. Note that this applies
strictly, also when it removes the last administrator (the server CLI is the way back, see break-glass).

### Client secret

The client secret is sealed in the credential store (`credentials`, kind `oidc`, the same key as S3
secrets) and never returned by any route (`has_secret` only); an empty `client_secret` on update keeps it,
`clear_secret: true` removes it (a public client using PKCE only).

### Testing a provider

*Test* (`POST /oidc/providers/{name}/test`) runs discovery against the issuer and fetches the key set:
issuer, endpoints, number of signing keys, PKCE methods, and warnings (no S256 listed, no
`email_verified` or groups claim listed). It also shows the redirect URI to register.

### Setup guides

The redirect URI is always `<base_url>/api/v1/auth/oidc/<name>/callback`; set `base_url` first. Use the
issuer exactly as the provider states it (with or without a trailing `/`): it must equal the `issuer` of
the provider's discovery document.

**Keycloak** (name `keycloak`):
1. Clients → *Create client*: type *OpenID Connect*, Client ID `incus-backup`; *Client authentication*
   ON (confidential), *Standard flow* ON, *Direct access grants* OFF.
2. *Valid redirect URIs*: `https://backup.example.com/api/v1/auth/oidc/keycloak/callback`; *Web origins*:
   `https://backup.example.com`. Advanced → *Proof Key for Code Exchange Code Challenge Method*: `S256`.
3. Credentials tab: copy the client secret.
4. Groups: Client scopes → `incus-backup-dedicated` → *Add mapper* → *By configuration* → *Group
   Membership*: Token Claim Name `groups`, *Full group path* OFF, *Add to ID token* ON.
5. Users' e-mail addresses must be verified (user → *Email verified*, or the realm's *Verify email*).
6. Here: issuer `https://id.example.com/realms/<realm>`, client id `incus-backup`, the secret, scopes
   `openid email profile`, groups claim `groups`, mappings e.g. `backup-admins → admin`,
   `backup-ops → operator`.

**Authentik** (name `authentik`):
1. Applications → Providers → *Create* → *OAuth2/OpenID Provider*: client type *Confidential*,
   redirect URI (strict) `https://backup.example.com/api/v1/auth/oidc/authentik/callback`, a signing key
   (an RSA certificate), scopes `openid`, `email`, `profile`.
2. Create an application with that provider (its slug is in the issuer).
3. The default `profile` scope mapping includes `groups` (group names). Check that the `email` scope
   mapping returns `email_verified: true` for your users (*Test* lists the claims the provider supports;
   sign-in is refused without it); if not, add a scope mapping that returns it for verified addresses.
4. Here: issuer `https://auth.example.com/application/o/<slug>/` (with the trailing `/`), client id and
   secret from the provider, groups claim `groups`.

**Google** (name `google`):
1. Google Cloud console → APIs & Services → Credentials → *OAuth client ID*, type *Web application*,
   authorized redirect URI `https://backup.example.com/api/v1/auth/oidc/google/callback`.
2. Here: issuer `https://accounts.google.com`, client id and secret, scopes `openid email profile`,
   **allowed e-mail domains** `example.com` (otherwise any Google account could sign in when automatic
   creation is on). Google sends no groups claim: use the default role (keep it low, e.g. `none` or
   `readonly`) and raise individual users by linking pre-created accounts, or turn automatic creation off
   and link every user explicitly.

**Microsoft Entra ID**: its ID tokens carry no `email_verified` claim, so sign-in is refused (fail closed).
Not supported in this release.

### Password and passkey sign-in off (`local_login`)

When single sign-on is the only way in, Settings → Authentication can turn off password **and passkey**
sign-in on the web listener (`PUT /auth/settings {"local_login": false}`; passkeys count as local
sign-in). The server refuses unless an enabled administrator is linked to an enabled provider and has
signed in through it (so somebody can still administer the server), and while it is off it also refuses
to disable or delete the last such provider or unlink the last such administrator. Sessions already open
continue; API tokens (of accounts with a local password) keep working; the unix-socket admin API is unaffected. Demoting, disabling or deleting
that administrator through the user editor is not guarded; `incus-backup-server user local-login on` is the
way back.

## Lost device and break-glass

All of these work on the server itself, on the database directly (no running daemon, no web sign-in):

| situation | fix |
|---|---|
| lost security key / passkey, or a key blocked as possibly copied | Users → … → *Remove security keys* (`DELETE /users/{name}/webauthn`), or `incus-backup-server user webauthn-reset NAME` |
| lost authenticator app and recovery codes | Users → … → *Remove two-factor* (`DELETE /users/{name}/totp`), or `incus-backup-server user disable-2fa NAME` |
| forgotten password | Users → *Password*, or `incus-backup-server user set-password NAME` |
| identity provider down while password sign-in is off | `incus-backup-server user local-login on`, then sign in with a password (set one with `user set-password` if needed) |
| nobody is administrator any more (e.g. a group mapping changed) | `incus-backup-server user create NAME --role admin` (or the unix admin socket) |

TOTP recovery codes keep working as before. Register two keys, or a key and an authenticator app, so one
loss does not need an administrator. Every reset is in the audit log.

## API summary

| route | who | notes |
|---|---|---|
| `POST /session` | public | a user with a key and no `code`: 401 `{totp_required, second_factor: {totp, webauthn}, ticket}` |
| `POST /session/webauthn/begin` | public | `{}`: passkey; `{ticket}`: second factor → `{ceremony, options}` |
| `POST /session/webauthn/finish` | public | `{ceremony, credential}` → the session (cookie + CSRF), as `POST /session` |
| `GET /auth/oidc/providers` | public | `{providers: [{name, label}], local_login, passkeys}` |
| `GET /auth/oidc/{provider}/login`, `/callback` | public | browser redirects |
| `GET /me/webauthn`, `POST /me/webauthn/register/begin|finish`, `PATCH/DELETE /me/webauthn/{id}` | any user | own keys |
| `DELETE /users/{name}/webauthn` | admin | reset keys |
| `GET/POST /users/{name}/identities`, `DELETE /users/{name}/identities/{provider}` | admin | provider links |
| `GET/POST /oidc/providers`, `PUT/DELETE /oidc/providers/{name}`, `POST /oidc/providers/{name}/test` | admin | providers |
| `GET/PUT /auth/settings` | admin | `require_2fa`, `local_login`, `webauthn_rp_id`, `webauthn_origins` (+ what is in effect) |

The sign-in policy keys are not accepted by the generic `PUT /settings` (it has no lock-out guard).

## Tests

- `internal/server/mgmt/authn_webauthn_test.go`: registration and sign-in through the HTTP routes with a
  software authenticator ([github.com/descope/virtualwebauthn](https://github.com/descope/virtualwebauthn):
  real ECDSA keys, real signatures over the server's challenges): second factor after the password, the
  password alone refused, single-use ticket, wrong origin, wrong RP ID, replayed ceremony and replayed
  assertion, expired ceremony, cross-user credential, counter regression and the blocked key, passkey
  sign-in with and without user verification, unknown and disabled users, re-authentication for
  registration, `require_2fa` and the last-factor guard, `local_login` refusing passwords and passkeys.
- `internal/server/mgmt/authn_oidc_test.go`: a real OpenID provider over HTTP
  ([github.com/oauth2-proxy/mockoidc](https://github.com/oauth2-proxy/mockoidc): discovery, JWKS, PKCE,
  RS256 ID tokens): auto-creation, role and repository mappings re-evaluated at each sign-in, scopes holding
  on `/repositories`, `/overview`, raw chunk and index routes and the event stream; refusals for state
  (forged, replayed, other browser), nonce, expired token, wrong audience, wrong issuer, broken signature,
  `email_verified` false, domain, unknown subject, a different subject with a linked address, name
  collision, disabled user, disabled provider; PKCE proven (challenge = S256(verifier), a wrong verifier
  fails); the client secret absent from every response and sealed at rest; the `local_login` lock-out guard.
- `internal/server/auth/authn_internal_test.go`: `next` sanitising, `email_verified` parsing, group claims,
  mapping arithmetic, provider validation, origins.
- `web/e2e/zzz-authn.real.spec.ts`: the real UI in Chromium against the real server over HTTPS on
  `localhost`, with Chrome's virtual authenticator (CDP) and a real local OpenID provider
  (`internal/webfixture/testidp`): register a key in the profile, passkey sign-in, password + key, adding a
  provider in the UI (the client secret never appears in a response), sign-in through it with the role
  from a group mapping, a refused sign-in, sign-out, forced enrolment, password sign-in off and the
  break-glass CLI.

Not covered by tests: real hardware keys, real identity providers (Keycloak, Authentik, Google), Safari and
Firefox, and a headed Chrome with a certificate error.
