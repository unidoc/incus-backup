// Saving verified plaintext streams to the user's disk.
//
// Integrity rule (web-crypto README): only a stream that CLOSED normally is complete; a stream that
// errors (a chunk or the whole-file SHA-256 failed, a sealed frame was altered) must not leave a file.
//  * small downloads are collected into a Blob and handed to the browser only after the stream ended;
//  * large downloads go straight to disk through the File System Access API when the browser has it
//    (bounded memory); on an error the partly written file is discarded (writable.abort()).

export interface SaveProgress { bytes: number }

/** Above this a download prefers streaming to disk (when the browser can). */
export const STREAM_TO_DISK_ABOVE = 256 * 1024 * 1024

interface FileHandleLike { createWritable(): Promise<{ write(b: Uint8Array): Promise<void>; close(): Promise<void>; abort(): Promise<void> }> }
type PickerWindow = Window & { showSaveFilePicker?: (o: { suggestedName?: string }) => Promise<FileHandleLike> }

export const canStreamToDisk = () => typeof window !== 'undefined' && typeof (window as PickerWindow).showSaveFilePicker === 'function'

/**
 * Asks for a target file. MUST be called synchronously from a click handler (user activation), before
 * any await. Returns null when the browser has no picker or the user cancelled.
 */
export async function pickSaveFile(name: string): Promise<FileHandleLike | null> {
  const w = window as PickerWindow
  if (!w.showSaveFilePicker) return null
  try { return await w.showSaveFilePicker({ suggestedName: name }) } catch { return null }
}

/** Reads a stream to the end, counting bytes; rejects on the stream's error (nothing is kept then). */
export async function collect(stream: ReadableStream<Uint8Array>, onBytes?: (n: number) => void, signal?: AbortSignal): Promise<Uint8Array[]> {
  const parts: Uint8Array[] = []
  let n = 0
  const r = stream.getReader()
  const stop = () => { r.cancel(signal?.reason).catch(() => undefined) }
  signal?.addEventListener('abort', stop, { once: true })
  try {
    for (;;) {
      const { done, value } = await r.read()
      if (signal?.aborted) throw signal.reason ?? new DOMException('aborted', 'AbortError')
      if (done) break
      parts.push(value)
      n += value.byteLength
      onBytes?.(n)
    }
  } finally {
    signal?.removeEventListener('abort', stop)
    r.releaseLock()
  }
  return parts
}

export function concat(parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((a, p) => a + p.byteLength, 0))
  let o = 0
  for (const p of parts) { out.set(p, o); o += p.byteLength }
  return out
}

/** Hands finished bytes to the browser as a download. */
export function saveBlob(parts: BlobPart[], name: string, type = 'application/octet-stream') {
  const url = URL.createObjectURL(new Blob(parts, { type }))
  const a = document.createElement('a')
  a.href = url
  a.download = name
  a.rel = 'noopener'
  a.style.display = 'none'
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 60_000)
}

/**
 * Saves a verified stream: into `handle` when given (streaming, discarded on error), else collected
 * and saved as a Blob once the stream closed normally. Resolves with the byte count.
 */
export async function saveStream(stream: ReadableStream<Uint8Array>, name: string, o: { handle?: FileHandleLike | null; onBytes?: (n: number) => void; signal?: AbortSignal; type?: string } = {}): Promise<number> {
  if (o.handle) {
    const w = await o.handle.createWritable()
    let n = 0
    const r = stream.getReader()
    const stop = () => { r.cancel(o.signal?.reason).catch(() => undefined) }
    o.signal?.addEventListener('abort', stop, { once: true })
    try {
      for (;;) {
        const { done, value } = await r.read()
        if (o.signal?.aborted) throw o.signal.reason ?? new DOMException('aborted', 'AbortError')
        if (done) break
        await w.write(value)
        n += value.byteLength
        o.onBytes?.(n)
      }
      await w.close()
      return n
    } catch (e) {
      await w.abort().catch(() => undefined)
      throw e
    } finally {
      o.signal?.removeEventListener('abort', stop)
    }
  }
  const parts = await collect(stream, o.onBytes, o.signal)
  saveBlob(parts as BlobPart[], name, o.type)
  return parts.reduce((a, p) => a + p.byteLength, 0)
}
