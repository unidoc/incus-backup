import { Core, defaultCore } from './core.js';
import { IbError } from './errors.js';

export interface KeyOptions {
  /** The Core to load the key into (default: the shared one, a Web Worker in a browser page). */
  core?: Core;
}

/**
 * An opened key domain. The derived sub-keys live inside the wasm module (in the worker); this
 * object holds only an integer handle and the non-secret domain id.
 */
export class KeyHandle {
  private open = true;
  /** @internal */
  constructor(
    readonly core: Core,
    /** @internal */ readonly handle: number,
    /** Non-secret key domain id (16 hex characters); the server records it per repository. */
    readonly domainId: string,
  ) {}

  get closed(): boolean {
    return !this.open;
  }

  /** @internal */
  check(): number {
    if (!this.open) throw new IbError('the key was closed');
    return this.handle;
  }

  /** The opaque group id the server stores an object under (keyed: only key holders can compute it). */
  groupId(project: string, kind: 'block' | 'filesystem' | 'instance' | string, name: string): Promise<string> {
    return this.core.groupId(this.check(), project, kind, name);
  }

  /** Wipes the sub-keys inside the module. Idempotent. */
  async close(): Promise<void> {
    if (!this.open) return;
    this.open = false;
    await this.core.dropKey(this.handle);
  }
}

/**
 * Opens a recovery key ("IBR1-XXXXX-..."; case, spaces and dashes are ignored). Rejects with a
 * KeyError ("recovery key checksum mismatch (typo?)", "... must start with IBR1", ...) when it is
 * not valid. The string is handed to the module once and not kept; JavaScript cannot wipe
 * strings, so clear the input field after calling this.
 */
export async function openKey(recoveryKey: string, opts: KeyOptions = {}): Promise<KeyHandle> {
  const core = opts.core ?? defaultCore();
  const r = await core.loadRecoveryKey(recoveryKey);
  return new KeyHandle(core, r.key, r.domainId);
}

/**
 * Opens a key from an escrow blob (format.WrapMasterKey, "IBW1...") and the offline escrow
 * private key ("IBE1-..."). The master key is unwrapped inside the module.
 */
export async function openEscrowKey(wrapped: Uint8Array, escrowPrivateKey: string, opts: KeyOptions = {}): Promise<KeyHandle> {
  const core = opts.core ?? defaultCore();
  const r = await core.loadEscrow(wrapped, escrowPrivateKey);
  return new KeyHandle(core, r.key, r.domainId);
}

/** Wipes every key, catalog and stream state held by the module of `core` (default: the shared one). */
export async function zeroize(core: Core = defaultCore()): Promise<void> {
  await core.zeroize();
}
