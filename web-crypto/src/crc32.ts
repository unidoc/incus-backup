// CRC-32 (IEEE 802.3, as used by ZIP), slicing-by-8.

const T = (() => {
  const t = new Uint32Array(8 * 256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  for (let n = 0; n < 256; n++) {
    let c = t[n]!;
    for (let k = 1; k < 8; k++) {
      c = t[c & 0xff]! ^ (c >>> 8);
      t[k * 256 + n] = c >>> 0;
    }
  }
  return t;
})();

/** Continues a CRC-32: pass 0 to start, the previous result to continue. */
export function crc32(data: Uint8Array, crc = 0): number {
  let c = ~crc >>> 0;
  let i = 0;
  const n = data.length;
  for (; i + 8 <= n; i += 8) {
    const a = (data[i]! | (data[i + 1]! << 8) | (data[i + 2]! << 16) | (data[i + 3]! << 24)) ^ c;
    c =
      T[7 * 256 + (a & 0xff)]! ^
      T[6 * 256 + ((a >>> 8) & 0xff)]! ^
      T[5 * 256 + ((a >>> 16) & 0xff)]! ^
      T[4 * 256 + (a >>> 24)]! ^
      T[3 * 256 + data[i + 4]!]! ^
      T[2 * 256 + data[i + 5]!]! ^
      T[256 + data[i + 6]!]! ^
      T[data[i + 7]!]!;
  }
  for (; i < n; i++) c = T[(c ^ data[i]!) & 0xff]! ^ (c >>> 8);
  return ~c >>> 0;
}
