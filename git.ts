// git smart HTTP layer for the forge: pkt-lines, packs (build + parse, delta apply),
// upload-pack (v2 ls-refs/fetch, v0 fetch) and receive-pack (v0, report-status).
import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export type ObjType = 'blob' | 'tree' | 'commit' | 'tag';
export interface GitObj { type: ObjType; data: Buffer }
export const ZERO = '0000000000000000000000000000000000000000';
const TYPE_CODE: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

// ---------------------------------------------------------------- pkt-lines
export function pktLine(payload: Buffer | string): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = data.length + 4;
  const head = Buffer.from(len.toString(16).padStart(4, '0'), 'latin1');
  return Buffer.concat([head, data]);
}
export const PKT_FLUSH = Buffer.from('0000', 'latin1');
export const PKT_DELIM = Buffer.from('0001', 'latin1');
export type Packet = { kind: 'data'; payload: Buffer } | { kind: 'flush' } | { kind: 'delim' } | { kind: 'end' };
export function decodePkts(buf: Buffer, offset = 0): { packets: Packet[]; rest: number } {
  const packets: Packet[] = [];
  let i = offset;
  while (i + 4 <= buf.length) {
    const lenHex = buf.subarray(i, i + 4).toString('latin1');
    if (!/^[0-9a-f]{4}$/.test(lenHex)) break;
    const len = parseInt(lenHex, 16);
    if (len === 0) { packets.push({ kind: 'flush' }); i += 4; break; }
    if (len === 1) { packets.push({ kind: 'delim' }); i += 4; continue; }
    if (len === 2) { packets.push({ kind: 'end' }); i += 4; break; }
    if (len < 4 || i + len > buf.length) break;
    packets.push({ kind: 'data', payload: buf.subarray(i + 4, i + len) });
    i += len;
  }
  return { packets, rest: i };
}

// ---------------------------------------------------------------- packs
export function buildPack(objects: { type: ObjType; data: Buffer }[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'latin1'); header.writeUInt32BE(2, 4); header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const obj of objects) {
    let size = obj.data.length;
    let first = (TYPE_CODE[obj.type] << 4) | (size & 0x0f);
    size >>= 4;
    const bytes: number[] = [];
    while (size > 0) { bytes.push(first | 0x80); first = size & 0x7f; size >>= 7; }
    bytes.push(first);
    parts.push(Buffer.from(bytes));
    parts.push(deflateSync(obj.data));
  }
  const body = Buffer.concat(parts);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let i = 0;
  const readVarint = (): number => {
    let v = 0; let shift = 0;
    for (;;) { const b = delta[i++]; v |= (b & 0x7f) << shift; shift += 7; if (!(b & 0x80)) break; }
    return v;
  };
  readVarint(); // source size
  const targetSize = readVarint();
  const out = Buffer.alloc(targetSize);
  let written = 0;
  while (i < delta.length) {
    const op = delta[i++];
    if (op & 0x80) {
      let off = 0; let size = 0;
      if (op & 0x01) off = delta[i++];
      if (op & 0x02) off |= delta[i++] << 8;
      if (op & 0x04) off |= delta[i++] << 16;
      if (op & 0x08) off |= delta[i++] << 24;
      if (op & 0x10) size = delta[i++];
      if (op & 0x20) size |= delta[i++] << 8;
      if (op & 0x40) size |= delta[i++] << 16;
      if (size === 0) size = 0x10000;
      base.copy(out, written, off, off + size);
      written += size;
    } else if (op > 0) {
      delta.copy(out, written, i, i + op);
      i += op; written += op;
    } else throw new Error('delta opcode 0');
  }
  return out;
}

export interface ParsedPack { objects: Map<string, GitObj>; count: number }
/** Inflate one zlib stream at `offset`; returns its bytes and the offset just past the stream. */
export function inflateAt(buffer: Buffer, offset: number): { data: Buffer; next: number } {
  const result = inflateSync(buffer.subarray(offset), { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
  return { data: result.buffer, next: offset + result.engine.bytesWritten };
}
/** Parse a pack into resolved objects. `external` resolves ref-delta bases outside the pack. */
export function parsePack(buf: Buffer, external?: (sha: string) => GitObj | undefined): ParsedPack {
  if (buf.length < 12 || buf.subarray(0, 4).toString('latin1') !== 'PACK') throw new Error('bad pack header');
  const count = buf.readUInt32BE(8);
  let i = 12;
  interface RawEntry { offset: number; type: ObjType; data?: Buffer; deltaOfs?: number; deltaRef?: string; rawDelta?: Buffer }
  const raw: RawEntry[] = [];
  const byOffset = new Map<number, number>();
  for (let n = 0; n < count; n++) {
    const start = i;
    let b = buf[i++];
    const typeCode = (b >> 4) & 7;
    let size = b & 0x0f; let shift = 4;
    while (b & 0x80) { b = buf[i++]; size |= (b & 0x7f) << shift; shift += 7; }
    if (typeCode === 6) {
      let c = buf[i++];
      let off = c & 0x7f;
      while (c & 0x80) { c = buf[i++]; off = ((off + 1) << 7) | (c & 0x7f); }
      const inflated = inflateAt(buf, i);
      raw.push({ offset: start, type: 'blob', deltaOfs: start - off, rawDelta: inflated.data });
      i = inflated.next;
    } else if (typeCode === 7) {
      const ref = buf.subarray(i, i + 20).toString('hex'); i += 20;
      const inflated = inflateAt(buf, i);
      raw.push({ offset: start, type: 'blob', deltaRef: ref, rawDelta: inflated.data });
      i = inflated.next;
    } else {
      const inflated = inflateAt(buf, i);
      raw.push({ offset: start, type: CODE_TYPE[typeCode], data: inflated.data });
      i = inflated.next;
    }
    byOffset.set(start, raw.length - 1);
  }
  const shaOf = (type: ObjType, data: Buffer): string => {
    const head = Buffer.from(`${type} ${data.length}\0`, 'latin1');
    return createHash('sha1').update(Buffer.concat([head, data])).digest('hex');
  };
  const resolved = new Map<string, GitObj>();
  const memo = new Map<number, GitObj>();
  const resolveIdx = (idx: number): GitObj => {
    const hit = memo.get(idx); if (hit) return hit;
    const entry = raw[idx];
    let obj: GitObj;
    if (entry.data && !entry.rawDelta) obj = { type: entry.type, data: entry.data };
    else {
      let base: GitObj;
      if (entry.deltaOfs !== undefined) base = resolveIdx(byOffset.get(entry.deltaOfs)!);
      else {
        let found = -1;
        for (let j = 0; j < raw.length; j++) { const r = raw[j]; if (r.data && !r.rawDelta && shaOf(r.type, r.data) === entry.deltaRef) { found = j; break; } }
        if (found >= 0) base = resolveIdx(found);
        else { const ext = external?.(entry.deltaRef!); if (!ext) throw new Error('delta base missing'); base = ext; }
      }
      obj = { type: base.type, data: applyDelta(base.data, entry.rawDelta!) };
    }
    memo.set(idx, obj);
    return obj;
  };
  for (let idx = 0; idx < raw.length; idx++) {
    const obj = resolveIdx(idx);
    resolved.set(shaOf(obj.type, obj.data), obj);
  }
  return { objects: resolved, count };
}
