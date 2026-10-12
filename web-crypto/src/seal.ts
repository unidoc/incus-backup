// Sealed deliveries (format "IBS2", internal/format/sealedbox.go): files a host encrypts to a
// one-time X25519 key this page created AND authenticates with the repository key, for one job and
// the exact parameters this page sent. The relaying server can neither read them nor forge, alter,
// truncate, reorder or swap them: opening needs the one-time private key (inside the module, never
// in JavaScript) and the repository key (the recovery key, loaded with openKey).
import { Core, defaultCore } from './core.js';
import { abortError, IbError, KeyError } from './errors.js';
import type { KeyHandle } from './key.js';

/** A one-time key pair for one delivery. The private half lives inside the module only. */
export class SealKey {
  private open = true;
  /** @internal */
  constructor(
    readonly core: Core,
    /** @internal */ readonly handle: number,
    /** 32-byte X25519 public key: send it to the host as recipient_public_key. */
    readonly publicKey: Uint8Array,
  ) {}

  get closed(): boolean {
    return !this.open;
  }

  /** @internal */
  check(): number {
    if (!this.open) throw new KeyError('the delivery key was discarded');
    return this.handle;
  }

  /** Discards the private key inside the module: the delivery can no longer be opened. Idempotent. */
  async close(): Promise<void> {
    if (!this.open) return;
    this.open = false;
    await this.core.sealDrop(this.handle).catch(() => undefined);
  }
}

/** The one-time key pair of a delivery (the private half stays inside the module). */
export type SealKeypair = SealKey;

/** A fresh one-time X25519 key pair (crypto/ecdh inside the module, randomness from crypto.getRandomValues). */
export async function generateSealKeypair(opts: { core?: Core } = {}): Promise<SealKey> {
  const core = opts.core ?? defaultCore();
  const r = await core.sealNew();
  return new SealKey(core, r.seal, r.publicKey);
}

/**
 * Moves an existing X25519 private key into the module (interoperability and tests). The given
 * bytes are wiped (filled with zeros) by this call.
 */
export async function importSealKey(privateKey: Uint8Array, opts: { core?: Core } = {}): Promise<SealKey> {
  if (!(privateKey instanceof Uint8Array) || privateKey.length !== 32) throw new KeyError('the private key must be 32 bytes');
  const core = opts.core ?? defaultCore();
  const copy = privateKey.slice();
  privateKey.fill(0);
  try {
    const r = await core.sealImport(copy);
    return new SealKey(core, r.seal, r.publicKey);
  } finally {
    copy.fill(0);
  }
}

/** The request a delivery answers: the job id the server issued and the parameters THIS page sent. */
export interface DeliveryRequest {
  /** Job id (from POST /jobs). */
  jobId: string;
  /** Job type, e.g. "fs_extract" or "guest_files". */
  type: string;
  /** The parameters exactly as sent in POST /jobs (an object or its JSON). */
  params: unknown;
}

/** jobspec.RequestDigest of a request (the same Go validation the host runs). */
export async function requestDigest(type: string, params: unknown, opts: { core?: Core } = {}): Promise<Uint8Array> {
  const json = typeof params === 'string' ? params : JSON.stringify(params);
  return (opts.core ?? defaultCore()).requestDigest(type, json);
}

export interface UnsealOptions {
  /** The repository key (openKey with the recovery key): only its holders can produce a delivery this accepts. */
  key: KeyHandle;
  /** The one-time key the job's recipient_public_key came from. */
  seal: SealKey;
  /** What was asked for. */
  request: DeliveryRequest;
  signal?: AbortSignal;
}

/**
 * Verifies and decrypts a sealed delivery. The output errors (IntegrityError) when the input is
 * forged, made for another job or other parameters, altered, reordered, truncated or has data after
 * its final frame. The newest frame is held back until the next one verified, so the last piece of
 * plaintext arrives only together with the authenticated trailer; the stream CLOSES only after the
 * whole delivery verified. Consumers must treat everything as incomplete until the stream closed.
 * Memory: about two frames (2 MiB) plus one input piece.
 */
export function unsealStream(sealed: ReadableStream<Uint8Array> | Uint8Array, o: UnsealOptions): ReadableStream<Uint8Array> {
  const signal = o?.signal;
  const src = sealed instanceof Uint8Array ? null : sealed.getReader();
  let bytes: Uint8Array | null = sealed instanceof Uint8Array ? sealed : null;
  let u: number | undefined;
  let core: Core | undefined;
  let finished = false;
  const PIECE = 1 << 20;
  const cleanup = async () => {
    if (u !== undefined && core) {
      const h = u;
      u = undefined;
      await core.unsealClose(h).catch(() => undefined);
    }
  };
  return new ReadableStream<Uint8Array>(
    {
      async pull(ctrl) {
        try {
          if (signal?.aborted) throw abortError(signal);
          if (u === undefined) {
            if (!o || !o.key || !o.seal || !o.request) throw new IbError('unsealStream needs the repository key, the delivery key and the request');
            if (o.key.core !== o.seal.core) throw new IbError('the repository key and the delivery key belong to different modules');
            core = o.key.core;
            const digest = await requestDigest(o.request.type, o.request.params, { core });
            u = await core.unsealOpen(o.key.check(), o.seal.check(), o.request.jobId, digest);
          }
          const c = core!;
          for (;;) {
            if (signal?.aborted) throw abortError(signal);
            let chunk: Uint8Array | undefined;
            if (bytes) {
              chunk = bytes.subarray(0, PIECE);
              bytes = bytes.length > PIECE ? bytes.subarray(PIECE) : null;
              if (!bytes) finished = true;
            } else if (src && !finished) {
              const r = await src.read();
              if (r.done) finished = true;
              else chunk = r.value;
            } else {
              finished = true;
            }
            if (chunk && chunk.length) {
              const out = await c.unsealPush(u!, chunk);
              if (out.length) {
                ctrl.enqueue(out);
                if (!finished) return;
              }
            }
            if (finished) {
              const h = u!;
              u = undefined;
              await c.unsealEnd(h);
              ctrl.close();
              return;
            }
          }
        } catch (e) {
          await cleanup();
          if (src) src.cancel(e).catch(() => undefined);
          ctrl.error(e);
        }
      },
      async cancel(reason) {
        await cleanup();
        if (src) await src.cancel(reason).catch(() => undefined);
      },
    },
    { highWaterMark: 0 },
  );
}
