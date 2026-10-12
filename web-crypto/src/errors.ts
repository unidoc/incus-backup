/** Base class of every error this package raises. Messages never contain key material. */
export class IbError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'IbError';
  }
}

/** The recovery key (or escrow key) was rejected: bad checksum, wrong length, wrong prefix. */
export class KeyError extends IbError {
  constructor(message: string) {
    super(message);
    this.name = 'KeyError';
  }
}

/**
 * Repository data failed verification: a manifest, index, chunk or catalog that does not
 * authenticate (wrong key, or a server that altered, swapped or truncated data), or a file whose
 * content does not match its recorded SHA-256.
 */
export class IntegrityError extends IbError {
  constructor(message: string) {
    super(message);
    this.name = 'IntegrityError';
  }
}

/** The server (fetcher) failed to deliver an object. */
export class FetchError extends IbError {
  constructor(
    message: string,
    readonly status?: number,
  ) {
    super(message);
    this.name = 'FetchError';
  }
}

export function abortError(signal?: AbortSignal): Error {
  const r = signal?.reason;
  if (r instanceof Error) return r;
  return new DOMException('The operation was aborted', 'AbortError');
}

export function throwIfAborted(signal?: AbortSignal): void {
  if (signal?.aborted) throw abortError(signal);
}
