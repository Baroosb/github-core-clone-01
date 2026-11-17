// github-core-clone-01 forge — V1: REST v3 surface + wire protocol over an in-memory
// store with a real git object model (contents/commits/branches are git objects).
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { createHash } from 'node:crypto';

const JSON_TYPE = 'application/json; charset=utf-8';
const VIEWER = { id: 1000, login: 'adaiversen', name: 'ada iversen', email: 'adaiversen@example.invalid' };
const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';
const EPOCH = Date.UTC(2025, 0, 1) / 1000;

let clockTick = 0;
/** Deterministic timestamp: fixed epoch + a per-process counter (replay-safe). */
function nextTs(): number { clockTick += 1; return EPOCH + clockTick; }
function isoOf(ts: number): string { return new Date(ts * 1000).toISOString().replace(/\.\d+Z$/, 'Z'); }

// ---------------------------------------------------------------- git objects
type ObjType = 'blob' | 'tree' | 'commit' | 'tag';
interface GitObj { type: ObjType; data: Buffer }

function sha1Of(type: ObjType, data: Buffer): string {
  const head = Buffer.from(`${type} ${data.length}\0`, 'latin1');
  return createHash('sha1').update(Buffer.concat([head, data])).digest('hex');
}

interface TreeEntry { mode: string; name: string; sha: string }
function parseTree(data: Buffer): TreeEntry[] {
  const out: TreeEntry[] = [];
  let i = 0;
  while (i < data.length) {
    const sp = data.indexOf(0x20, i);
    const mode = data.subarray(i, sp).toString('latin1');
    const nul = data.indexOf(0x00, sp);
    const name = data.subarray(sp + 1, nul).toString('utf8');
    const sha = data.subarray(nul + 1, nul + 21).toString('hex');
    out.push({ mode, name, sha });
    i = nul + 21;
  }
  return out;
}
function serializeTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const an = a.mode === '40000' ? `${a.name}/` : a.name;
    const bn = b.mode === '40000' ? `${b.name}/` : b.name;
    return an < bn ? -1 : an > bn ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.sha, 'hex'));
  }
  return Buffer.concat(parts);
}

interface CommitInfo { tree: string; parents: string[]; message: string; authorLine: string; committerLine: string }
function parseCommit(data: Buffer): CommitInfo {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split === -1 ? text : text.slice(0, split);
  const message = split === -1 ? '' : text.slice(split + 2);
  let tree = ''; const parents: string[] = []; let authorLine = ''; let committerLine = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) authorLine = line.slice(7);
    else if (line.startsWith('committer ')) committerLine = line.slice(10);
  }
  return { tree, parents, message, authorLine, committerLine };
}

// ---------------------------------------------------------------- store
interface IssueRec {
  kind: 'issue' | 'pull';
  number: number; id: number; title: string; body: string; state: 'open' | 'closed';
  labels: string[]; comments: { id: number; body: string }[];
  // V5 (read): mutation version + memoized serialized body for the single-issue GET.
  ver: number; cacheBuf?: Buffer; cacheVer?: number; cacheLabelVer?: number;
  // pull-only
  headRef?: string; baseRef?: string; merged?: boolean; mergeCommitSha?: string | null; mergedTs?: number;
  createdSeq: number;
}
interface Repo {
  id: number; name: string; isPrivate: boolean; description: string; defaultBranch: string;
  objects: Map<string, GitObj>; refs: Map<string, string>;
  labels: Map<string, { name: string; color: string }>; labelVer: number;
  // V8 (clone): single-entry memo of the last upload-pack fetch response. The fetch
  // response is a pure function of (object store, wants, haves, boundaries, depth,
  // protocol); objects are content-addressed, immutable, and only ever added within a
  // repo's lifetime, so an unchanged objects.size means an unchanged object set.
  fetchCacheKey?: string; fetchCacheBuf?: Buffer;
  issues: IssueRec[]; nextNumber: number; createdSeq: number;
}

let nextRepoId = 5000;
let nextIssueId = 100000;
let seq = 0;
const repos = new Map<string, Repo>();

function newRepo(name: string, isPrivate: boolean, description: string): Repo {
  const repo: Repo = {
    id: nextRepoId++, name, isPrivate, description, defaultBranch: 'main',
    objects: new Map(), refs: new Map(), labels: new Map(), labelVer: 0, issues: [], nextNumber: 1, createdSeq: ++seq,
  };
  repos.set(name, repo);
  return repo;
}
function storeObject(repo: Repo, type: ObjType, data: Buffer): string {
  const sha = sha1Of(type, data);
  if (!repo.objects.has(sha)) repo.objects.set(sha, { type, data });
  return sha;
}
function makeCommit(repo: Repo, tree: string, parents: string[], message: string): string {
  const ts = nextTs();
  const ident = `${VIEWER.name} <${VIEWER.email}> ${ts} +0000`;
  const lines = [`tree ${tree}`, ...parents.map((p) => `parent ${p}`), `author ${ident}`, `committer ${ident}`];
  return storeObject(repo, 'commit', Buffer.from(`${lines.join('\n')}\n\n${message.endsWith('\n') ? message : `${message}\n`}`, 'utf8'));
}
/** Tree sha at a commit (or the empty tree when the commit is undefined). */
function treeOfCommit(repo: Repo, commitSha: string | undefined): string {
  if (!commitSha) return storeObject(repo, 'tree', Buffer.alloc(0));
  const obj = repo.objects.get(commitSha);
  if (!obj || obj.type !== 'commit') return storeObject(repo, 'tree', Buffer.alloc(0));
  return parseCommit(obj.data).tree;
}
/** Read the blob sha at `path` inside a tree (recursive); undefined when absent. */
function blobAtPath(repo: Repo, treeSha: string, path: string): string | undefined {
  const parts = path.split('/');
  let cur = treeSha;
  for (let i = 0; i < parts.length; i++) {
    const obj = repo.objects.get(cur);
    if (!obj || obj.type !== 'tree') return undefined;
    const entry = parseTree(obj.data).find((e) => e.name === parts[i]);
    if (!entry) return undefined;
    if (i === parts.length - 1) return entry.mode === '40000' ? undefined : entry.sha;
    cur = entry.sha;
  }
  return undefined;
}
/** All file paths (recursive) of a tree: path -> blob sha. */
function flattenTree(repo: Repo, treeSha: string, prefix = ''): Map<string, string> {
  const out = new Map<string, string>();
  const obj = repo.objects.get(treeSha);
  if (!obj || obj.type !== 'tree') return out;
  for (const e of parseTree(obj.data)) {
    if (e.mode === '40000') for (const [k, v] of flattenTree(repo, e.sha, `${prefix}${e.name}/`)) out.set(k, v);
    else out.set(`${prefix}${e.name}`, e.sha);
  }
  return out;
}
/** Return a new tree sha with `path` set to blobSha (or removed when blobSha is null). */
function updateTree(repo: Repo, treeSha: string, parts: string[], blobSha: string | null): string {
  const obj = repo.objects.get(treeSha);
  const entries: TreeEntry[] = obj && obj.type === 'tree' ? parseTree(obj.data) : [];
  const [head, ...rest] = parts;
  const idx = entries.findIndex((e) => e.name === head);
  if (rest.length === 0) {
    if (blobSha === null) { if (idx >= 0) entries.splice(idx, 1); }
    else if (idx >= 0) entries[idx] = { mode: '100644', name: head, sha: blobSha };
    else entries.push({ mode: '100644', name: head, sha: blobSha });
  } else {
    const subSha = idx >= 0 && entries[idx].mode === '40000' ? entries[idx].sha : storeObject(repo, 'tree', Buffer.alloc(0));
    const newSub = updateTree(repo, subSha, rest, blobSha);
    const subObj = repo.objects.get(newSub);
    const empty = subObj && subObj.type === 'tree' && subObj.data.length === 0;
    if (empty) { if (idx >= 0) entries.splice(idx, 1); }
    else if (idx >= 0) entries[idx] = { mode: '40000', name: head, sha: newSub };
    else entries.push({ mode: '40000', name: head, sha: newSub });
  }
  return storeObject(repo, 'tree', serializeTree(entries));
}
function branchTip(repo: Repo, branch: string): string | undefined {
  return repo.refs.get(`refs/heads/${branch}`);
}
function isAncestor(repo: Repo, anc: string, desc: string): boolean {
  const seen = new Set<string>(); const stack = [desc];
  while (stack.length) {
    const cur = stack.pop()!;
    if (cur === anc) return true;
    if (seen.has(cur)) continue; seen.add(cur);
    const obj = repo.objects.get(cur);
    if (obj && obj.type === 'commit') stack.push(...parseCommit(obj.data).parents);
  }
  return false;
}
function mergeBase(repo: Repo, a: string, b: string): string | undefined {
  const ancOf = (start: string): Set<string> => {
    const out = new Set<string>(); const stack = [start];
    while (stack.length) { const cur = stack.pop()!; if (out.has(cur)) continue; out.add(cur); const o = repo.objects.get(cur); if (o && o.type === 'commit') stack.push(...parseCommit(o.data).parents); }
    return out;
  };
  const aa = ancOf(a);
  let cur: string | undefined = b;
  const seen = new Set<string>(); const queue = [b];
  while (queue.length) { cur = queue.shift()!; if (aa.has(cur)) return cur; if (seen.has(cur)) continue; seen.add(cur); const o = repo.objects.get(cur); if (o && o.type === 'commit') queue.push(...parseCommit(o.data).parents); }
  return undefined;
}
/** Three-way file-level merge of two tips; returns merged tree sha or null on conflict. */
function mergeTrees(repo: Repo, baseTip: string, headTip: string): string | null {
  const mb = mergeBase(repo, baseTip, headTip);
  const baseFiles = flattenTree(repo, treeOfCommit(repo, mb));
  const headFiles = flattenTree(repo, treeOfCommit(repo, headTip));
  const baseBranchFiles = flattenTree(repo, treeOfCommit(repo, baseTip));
  const merged = new Map<string, string>();
  const paths = new Set([...baseFiles.keys(), ...headFiles.keys(), ...baseBranchFiles.keys()]);
  for (const p of paths) {
    const inBase = baseFiles.get(p); const inHead = headFiles.get(p); const inBranch = baseBranchFiles.get(p);
    if (inHead === inBranch) { if (inHead !== undefined) merged.set(p, inHead); continue; }
    if (inBase === inBranch) { if (inHead !== undefined) merged.set(p, inHead); continue; }
    if (inBase === inHead) { if (inBranch !== undefined) merged.set(p, inBranch); continue; }
    return null; // both sides changed the same path differently
  }
  let tree = storeObject(repo, 'tree', Buffer.alloc(0));
  for (const [p, sha] of merged) tree = updateTree(repo, tree, p.split('/'), sha);
  return tree;
}

// ---------------------------------------------------------------- http plumbing
let requestCounter = 0;
function sendJsonText(res: ServerResponse, status: number, text: string, headers: Record<string, string> = {}): void {
  requestCounter += 1;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'x-github-request-id': `req-${requestCounter.toString(16).padStart(8, '0')}`,
    ...(text ? { 'content-length': Buffer.byteLength(text) } : {}),
    ...headers,
  });
  res.end(text);
}
function sendJsonBuffer(res: ServerResponse, status: number, buf: Buffer, headers: Record<string, string> = {}): void {
  requestCounter += 1;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'x-github-request-id': `req-${requestCounter.toString(16).padStart(8, '0')}`,
    'content-length': buf.length,
    ...headers,
  });
  res.end(buf);
}
function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  sendJsonText(res, status, body === undefined ? '' : `${JSON.stringify(body)}\n`, headers);
}
function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (c) => chunks.push(c as Buffer));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}
function authed(req: IncomingMessage): boolean {
  if (!TOKEN) return true;
  return (req.headers['authorization'] ?? '') === `Bearer ${TOKEN}`;
}

// ---------------------------------------------------------------- shapes
function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id, name: repo.name, full_name: `${VIEWER.login}/${repo.name}`,
    owner: { login: VIEWER.login, id: VIEWER.id, type: 'User' },
    private: repo.isPrivate, description: repo.description, default_branch: repo.defaultBranch,
    created_at: isoOf(EPOCH + repo.createdSeq), updated_at: isoOf(EPOCH + repo.createdSeq),
    html_url: `http://forge/${VIEWER.login}/${repo.name}`,
  };
}
function issueJson(repo: Repo, rec: IssueRec): Record<string, unknown> {
  const base: Record<string, unknown> = {
    id: rec.id, number: rec.number, title: rec.title, body: rec.body, state: rec.state,
    labels: rec.labels.map((name) => ({ name, color: repo.labels.get(name)?.color ?? 'ededed' })),
    user: { login: VIEWER.login, id: VIEWER.id, type: 'User' },
    comments: rec.comments.length,
    created_at: isoOf(EPOCH + rec.createdSeq), updated_at: isoOf(EPOCH + rec.createdSeq),
  };
  if (rec.kind === 'pull') base.pull_request = { url: `/api/v3/repos/${VIEWER.login}/${repo.name}/pulls/${rec.number}` };
  return base;
}
function pullJson(repo: Repo, rec: IssueRec): Record<string, unknown> {
  const headSha = rec.headRef ? branchTip(repo, rec.headRef) : undefined;
  const baseSha = rec.baseRef ? branchTip(repo, rec.baseRef) : undefined;
  let mergeable: boolean | null = null;
  if (rec.state === 'open' && headSha && baseSha) mergeable = mergeTrees(repo, baseSha, headSha) !== null;
  return {
    ...issueJson(repo, rec),
    state: rec.merged ? 'closed' : rec.state,
    head: { ref: rec.headRef, sha: headSha ?? null, label: `${VIEWER.login}:${rec.headRef}` },
    base: { ref: rec.baseRef, sha: baseSha ?? null, label: `${VIEWER.login}:${rec.baseRef}` },
    merged: !!rec.merged, merged_at: rec.merged ? isoOf(rec.mergedTs ?? EPOCH) : null,
    merge_commit_sha: rec.mergeCommitSha ?? null, mergeable,
    draft: false,
  };
}
function findRepo(owner: string, name: string): Repo | undefined {
  if (owner !== VIEWER.login) return undefined;
  return repos.get(name);
}
function parseJsonBody(raw: Buffer): { ok: true; value: Record<string, unknown> } | { ok: false } {
  if (raw.length === 0) return { ok: true, value: {} };
  try {
    const v = JSON.parse(raw.toString('utf8')) as unknown;
    if (typeof v !== 'object' || v === null || Array.isArray(v)) return { ok: false };
    return { ok: true, value: v as Record<string, unknown> };
  } catch { return { ok: false }; }
}
function paginate<T>(url: URL, items: T[], host: string): { page: T[]; link?: string } {
  const perPage = Math.max(1, Math.min(100, Number(url.searchParams.get('per_page') ?? 30) || 30));
  const pageNum = Math.max(1, Number(url.searchParams.get('page') ?? 1) || 1);
  const start = (pageNum - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  let link: string | undefined;
  if (start + perPage < items.length) {
    const mk = (p: number): string => {
      const u = new URL(url.toString());
      u.searchParams.set('page', String(p));
      u.searchParams.set('per_page', String(perPage));
      return `http://${host}${u.pathname}${u.search}`;
    };
    link = `<${mk(pageNum + 1)}>; rel="next", <${mk(Math.ceil(items.length / perPage))}>; rel="last"`;
  }
  return { page: slice, link };
}

// ---------------------------------------------------------------- rest helpers
function identParts(line: string): { name: string; email: string; date: string } {
  const m = /^(.*) <(.*)> (\d+) [+-]\d{4}$/.exec(line);
  if (!m) return { name: line, email: '', date: isoOf(EPOCH) };
  return { name: m[1], email: m[2], date: isoOf(Number(m[3])) };
}
function commitJson(repo: Repo, sha: string): Record<string, unknown> {
  const obj = repo.objects.get(sha);
  const info = obj && obj.type === 'commit' ? parseCommit(obj.data) : { tree: '', parents: [], message: '', authorLine: '', committerLine: '' };
  return {
    sha,
    commit: {
      tree: { sha: info.tree }, message: info.message.replace(/\n$/, ''),
      author: identParts(info.authorLine), committer: identParts(info.committerLine),
    },
    parents: info.parents.map((p) => ({ sha: p })),
    author: { login: VIEWER.login, id: VIEWER.id }, committer: { login: VIEWER.login, id: VIEWER.id },
  };
}
function listCommits(repo: Repo, fromSha: string): string[] {
  const seen = new Set<string>(); const stack = [fromSha]; const found: { sha: string; ts: number }[] = [];
  while (stack.length) {
    const cur = stack.pop()!;
    if (seen.has(cur)) continue; seen.add(cur);
    const obj = repo.objects.get(cur);
    if (!obj || obj.type !== 'commit') continue;
    const info = parseCommit(obj.data);
    const m = /(\d+) [+-]\d{4}/.exec(info.committerLine);
    found.push({ sha: cur, ts: m ? Number(m[1]) : 0 });
    stack.push(...info.parents);
  }
  found.sort((a, b) => b.ts - a.ts);
  return found.map((f) => f.sha);
}
function lineDiff(a: string, b: string): { additions: number; deletions: number } {
  const al = a === '' ? [] : a.split('\n'); const bl = b === '' ? [] : b.split('\n');
  // compact LCS
  const dp: number[] = new Array(bl.length + 1).fill(0);
  for (let i = 1; i <= al.length; i++) {
    let prev = 0;
    for (let j = 1; j <= bl.length; j++) {
      const tmp = dp[j];
      dp[j] = al[i - 1] === bl[j - 1] ? prev + 1 : Math.max(dp[j], dp[j - 1]);
      prev = tmp;
    }
  }
  const common = dp[bl.length];
  return { additions: bl.length - common, deletions: al.length - common };
}
function pullFiles(repo: Repo, rec: IssueRec): Record<string, unknown>[] {
  const headSha = rec.headRef ? branchTip(repo, rec.headRef) : undefined;
  const baseSha = rec.baseRef ? branchTip(repo, rec.baseRef) : undefined;
  if (!headSha || !baseSha) return [];
  const mb = mergeBase(repo, baseSha, headSha);
  const fromFiles = flattenTree(repo, treeOfCommit(repo, mb));
  const toFiles = flattenTree(repo, treeOfCommit(repo, headSha));
  const out: Record<string, unknown>[] = [];
  const paths = new Set([...fromFiles.keys(), ...toFiles.keys()]);
  for (const p of [...paths].sort()) {
    const from = fromFiles.get(p); const to = toFiles.get(p);
    if (from === to) continue;
    const fromText = from ? (repo.objects.get(from)?.data.toString('utf8') ?? '') : '';
    const toText = to ? (repo.objects.get(to)?.data.toString('utf8') ?? '') : '';
    const { additions, deletions } = lineDiff(fromText, toText);
    out.push({
      filename: p, status: from === undefined ? 'added' : to === undefined ? 'removed' : 'modified',
      additions, deletions, changes: additions + deletions,
      sha: to ?? from,
    });
  }
  return out;
}

// ---------------------------------------------------------------- the app
export function createApp(): Server {
  return createServer(async (req: IncomingMessage, res: ServerResponse) => {
    try {
      await route(req, res);
    } catch {
      if (!res.headersSent) sendJson(res, 500, { message: 'Internal Server Error' });
      else res.end();
    }
  });
}

async function route(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const url = new URL(req.url ?? '/', 'http://entry');
  const path = url.pathname;
  const method = req.method ?? 'GET';
  const raw = method === 'GET' || method === 'HEAD' ? Buffer.alloc(0) : await readBody(req);

  if (method === 'GET' && path === '/healthz') { sendJson(res, 200, { ok: true }); return; }

  if (await routeGit(req, res, url, method, raw)) return;

  if (path === '/api/graphql') {
    if (!authed(req)) { sendJson(res, 401, { message: 'Bad credentials' }); return; }
    if (method === 'POST') { handleGraphQL(res, raw); return; }
    if (method === 'GET') {
      const q = url.searchParams.get('query');
      if (q) { sendJson(res, 200, gqlRun(FORGE_SCHEMA, q, {})); return; }
    }
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }
  if (path === '/mcp') {
    if (!authed(req)) { sendJson(res, 401, { message: 'Bad credentials' }); return; }
    if (method === 'POST') { handleMcp(res, raw); return; }
    sendJson(res, 405, { message: 'Method Not Allowed' });
    return;
  }

  const isApi = path === '/api/v3' || path.startsWith('/api/v3/');
  if (isApi) {
    if (!authed(req)) { sendJson(res, 401, { message: 'Bad credentials' }); return; }
    await routeApi(req, res, url, method, raw);
    return;
  }
  sendJson(res, 404, { message: 'Not Found' });
}

async function routeApi(req: IncomingMessage, res: ServerResponse, url: URL, method: string, raw: Buffer): Promise<void> {
  const path = url.pathname;
  const seg = path.split('/').filter((s) => s.length > 0); // ['api','v3',...]
  const json = (): { ok: true; value: Record<string, unknown> } | { ok: false } => parseJsonBody(raw);
  const badJson = (): void => sendJson(res, 400, { message: 'Problems parsing JSON' });
  const notFound = (): void => sendJson(res, 404, { message: 'Not Found' });

  if (path === '/api/v3' || path === '/api/v3/') {
    if (method !== 'GET') return notFound();
    sendJson(res, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
    return;
  }
  if (path === '/api/v3/meta' && method === 'GET') { sendJson(res, 200, { installed_version: '1.0.0-forge', api_versions: ['3'] }); return; }
  if (path === '/api/v3/user' && method === 'GET') { sendJson(res, 200, { login: VIEWER.login, id: VIEWER.id, name: VIEWER.name, email: VIEWER.email, type: 'User' }); return; }

  if (path === '/api/v3/user/repos') {
    if (method === 'GET') {
      const all = [...repos.values()].sort((a, b) => a.createdSeq - b.createdSeq).map(repoJson);
      const { page, link } = paginate(url, all, req.headers.host ?? 'localhost');
      sendJson(res, 200, page, link ? { link } : {});
      return;
    }
    if (method === 'POST') {
      const body = json(); if (!body.ok) return badJson();
      const name = String(body.value.name ?? '');
      if (!name) { sendJson(res, 422, { message: 'Validation Failed: name is required' }); return; }
      if (repos.has(name)) { sendJson(res, 422, { message: 'Validation Failed: name already exists' }); return; }
      const repo = newRepo(name, Boolean(body.value.private), String(body.value.description ?? ''));
      if (typeof body.value.default_branch === 'string' && body.value.default_branch) repo.defaultBranch = body.value.default_branch;
      sendJson(res, 201, repoJson(repo));
      return;
    }
    return notFound();
  }

  // /api/v3/repos/{owner}/{repo}/...
  if (seg[0] === 'api' && seg[1] === 'v3' && seg[2] === 'repos' && seg.length >= 5) {
    const owner = decodeURIComponent(seg[3]); const name = decodeURIComponent(seg[4]);
    const repo = findRepo(owner, name);
    const rest = seg.slice(5).map((s) => decodeURIComponent(s));
    if (rest.length === 0) {
      if (!repo) return notFound();
      if (method === 'GET') { sendJson(res, 200, repoJson(repo)); return; }
      if (method === 'PATCH') {
        const body = json(); if (!body.ok) return badJson();
        if (typeof body.value.description === 'string') repo.description = body.value.description;
        if (typeof body.value.private === 'boolean') repo.isPrivate = body.value.private;
        if (typeof body.value.default_branch === 'string' && body.value.default_branch) repo.defaultBranch = body.value.default_branch;
        sendJson(res, 200, repoJson(repo)); return;
      }
      if (method === 'DELETE') { repos.delete(name); sendJson(res, 204, undefined); return; }
      return notFound();
    }
    if (!repo) return notFound();

    if (rest[0] === 'branches' && method === 'GET') {
      const out = [...repo.refs.entries()].filter(([r]) => r.startsWith('refs/heads/'))
        .map(([r, sha]) => ({ name: r.slice('refs/heads/'.length), commit: { sha }, protected: false }));
      sendJson(res, 200, out); return;
    }
    if (rest[0] === 'git' && rest[1] === 'refs' && method === 'POST') {
      const body = json(); if (!body.ok) return badJson();
      const ref = String(body.value.ref ?? ''); const sha = String(body.value.sha ?? '');
      if (!ref || !repo.objects.has(sha)) { sendJson(res, 422, { message: 'Validation Failed' }); return; }
      repo.refs.set(ref, sha);
      sendJson(res, 201, { ref, object: { sha, type: repo.objects.get(sha)!.type } });
      return;
    }
    if (rest[0] === 'commits' && method === 'GET') {
      const shaParam = url.searchParams.get('sha');
      const tip = shaParam ? (branchTip(repo, shaParam) ?? (repo.objects.has(shaParam) ? shaParam : undefined)) : branchTip(repo, repo.defaultBranch);
      if (!tip) { sendJson(res, 200, []); return; }
      const all = listCommits(repo, tip).map((s) => commitJson(repo, s));
      const { page, link } = paginate(url, all, req.headers.host ?? 'localhost');
      sendJson(res, 200, page, link ? { link } : {});
      return;
    }
    if (rest[0] === 'contents') {
      await routeContents(res, repo, rest.slice(1).join('/'), url, method, raw);
      return;
    }
    if (rest[0] === 'labels') {
      if (method === 'POST' && rest.length === 1) {
        const body = json(); if (!body.ok) return badJson();
        const lname = String(body.value.name ?? '');
        if (!lname) { sendJson(res, 422, { message: 'Validation Failed' }); return; }
        const color = String(body.value.color ?? 'ededed');
        repo.labels.set(lname, { name: lname, color });
        repo.labelVer += 1; // V5b: label colors feed issueJson (line ~252) — invalidate cached issue bodies repo-wide
        sendJson(res, 201, { name: lname, color, description: body.value.description ?? null });
        return;
      }
      if (method === 'GET' && rest.length === 1) {
        sendJson(res, 200, [...repo.labels.values()].map((l) => ({ name: l.name, color: l.color })));
        return;
      }
      return notFound();
    }
    if (rest[0] === 'issues') { routeIssues(res, repo, rest, url, method, raw, req.headers.host ?? 'localhost'); return; }
    if (rest[0] === 'pulls') { routePulls(res, repo, rest, url, method, raw, req.headers.host ?? 'localhost'); return; }
    return notFound();
  }

  return notFound();
}

async function routeContents(res: ServerResponse, repo: Repo, filePath: string, url: URL, method: string, raw: Buffer): Promise<void> {
  const notFound = (): void => sendJson(res, 404, { message: 'Not Found' });
  if (method === 'GET') {
    const ref = url.searchParams.get('ref') ?? repo.defaultBranch;
    const tip = branchTip(repo, ref) ?? (repo.objects.has(ref) ? ref : undefined);
    if (!tip) return notFound();
    const treeSha = treeOfCommit(repo, tip);
    if (filePath === '') {
      const entries = parseTree(repo.objects.get(treeSha)?.data ?? Buffer.alloc(0)).map((e) => ({
        name: e.name, path: e.name, sha: e.sha, type: e.mode === '40000' ? 'dir' : 'file',
        size: e.mode === '40000' ? 0 : (repo.objects.get(e.sha)?.data.length ?? 0),
      }));
      sendJson(res, 200, entries); return;
    }
    // directory?
    const parts = filePath.split('/');
    let cur = treeSha; let entry: TreeEntry | undefined;
    for (const p of parts) {
      const obj = repo.objects.get(cur);
      if (!obj || obj.type !== 'tree') { entry = undefined; break; }
      entry = parseTree(obj.data).find((e) => e.name === p);
      if (!entry) break;
      cur = entry.sha;
    }
    if (!entry) return notFound();
    if (entry.mode === '40000') {
      const entries = parseTree(repo.objects.get(entry.sha)?.data ?? Buffer.alloc(0)).map((e) => ({
        name: e.name, path: `${filePath}/${e.name}`, sha: e.sha, type: e.mode === '40000' ? 'dir' : 'file',
        size: e.mode === '40000' ? 0 : (repo.objects.get(e.sha)?.data.length ?? 0),
      }));
      sendJson(res, 200, entries); return;
    }
    const blob = repo.objects.get(entry.sha);
    if (!blob) return notFound();
    sendJson(res, 200, {
      type: 'file', encoding: 'base64', content: blob.data.toString('base64'),
      sha: entry.sha, size: blob.data.length, name: parts[parts.length - 1], path: filePath,
    });
    return;
  }
  const body = parseJsonBody(raw);
  if (!body.ok) { sendJson(res, 400, { message: 'Problems parsing JSON' }); return; }
  const branch = typeof body.value.branch === 'string' && body.value.branch ? body.value.branch : repo.defaultBranch;
  const tip = branchTip(repo, branch);
  const treeSha = treeOfCommit(repo, tip);
  const current = blobAtPath(repo, treeSha, filePath);
  if (method === 'PUT') {
    const givenSha = typeof body.value.sha === 'string' ? body.value.sha : undefined;
    if (current !== undefined && givenSha !== undefined && givenSha !== current) { sendJson(res, 409, { message: 'Conflict: sha does not match' }); return; }
    if (current !== undefined && givenSha === undefined) { sendJson(res, 422, { message: 'Validation Failed: sha is required to update' }); return; }
    const data = Buffer.from(String(body.value.content ?? ''), 'base64');
    const blobSha = storeObject(repo, 'blob', data);
    const newTree = updateTree(repo, treeSha, filePath.split('/'), blobSha);
    const commitSha = makeCommit(repo, newTree, tip ? [tip] : [], String(body.value.message ?? 'update'));
    repo.refs.set(`refs/heads/${branch}`, commitSha);
    sendJson(res, current === undefined ? 201 : 200, {
      content: { name: filePath.split('/').pop(), path: filePath, sha: blobSha, size: data.length, encoding: 'base64' },
      commit: { sha: commitSha },
    });
    return;
  }
  if (method === 'DELETE') {
    if (current === undefined) return notFound();
    const givenSha = typeof body.value.sha === 'string' ? body.value.sha : undefined;
    if (givenSha !== undefined && givenSha !== current) { sendJson(res, 409, { message: 'Conflict: sha does not match' }); return; }
    const newTree = updateTree(repo, treeSha, filePath.split('/'), null);
    const commitSha = makeCommit(repo, newTree, tip ? [tip] : [], String(body.value.message ?? 'delete'));
    repo.refs.set(`refs/heads/${branch}`, commitSha);
    sendJson(res, 200, { content: null, commit: { sha: commitSha } });
    return;
  }
  return notFound();
}

function routeIssues(res: ServerResponse, repo: Repo, rest: string[], url: URL, method: string, raw: Buffer, host: string): void {
  const notFound = (): void => sendJson(res, 404, { message: 'Not Found' });
  if (rest.length === 1) {
    if (method === 'POST') {
      const body = parseJsonBody(raw); if (!body.ok) { sendJson(res, 400, { message: 'Problems parsing JSON' }); return; }
      const labels = Array.isArray(body.value.labels) ? body.value.labels.map(String) : [];
      for (const l of labels) if (!repo.labels.has(l)) { sendJson(res, 422, { message: `Validation Failed: label ${l} does not exist` }); return; }
      const rec: IssueRec = {
        kind: 'issue', number: repo.nextNumber++, id: nextIssueId++,
        title: String(body.value.title ?? ''), body: String(body.value.body ?? ''),
        state: 'open', labels, comments: [], createdSeq: ++seq, ver: 0,
      };
      repo.issues.push(rec);
      sendJson(res, 201, issueJson(repo, rec));
      return;
    }
    if (method === 'GET') {
      const state = url.searchParams.get('state') ?? 'open';
      const direction = url.searchParams.get('direction') ?? 'asc';
      let list = repo.issues.filter((i) => i.kind === 'issue');
      if (state !== 'all') list = list.filter((i) => i.state === state);
      list = [...list].sort((a, b) => (direction === 'desc' ? b.createdSeq - a.createdSeq : a.createdSeq - b.createdSeq));
      const { page, link } = paginate(url, list.map((i) => issueJson(repo, i)), host);
      sendJson(res, 200, page, link ? { link } : {});
      return;
    }
    return notFound();
  }
  const num = Number(rest[1]);
  const rec = repo.issues.find((i) => i.kind === 'issue' && i.number === num);
  if (rest.length === 2) {
    if (!rec) return notFound();
    if (method === 'GET') {
      // V7 (read): serve the memoized body as a pre-encoded Buffer while the record is
      // unmutated, so res.end skips the per-request UTF-8 encoding of the same string.
      if (rec.cacheBuf !== undefined && rec.cacheVer === rec.ver && rec.cacheLabelVer === repo.labelVer) { sendJsonBuffer(res, 200, rec.cacheBuf); return; }
      const buf = Buffer.from(`${JSON.stringify(issueJson(repo, rec))}\n`);
      rec.cacheBuf = buf; rec.cacheVer = rec.ver; rec.cacheLabelVer = repo.labelVer;
      sendJsonBuffer(res, 200, buf);
      return;
    }
    if (method === 'PATCH') {
      const body = parseJsonBody(raw); if (!body.ok) { sendJson(res, 400, { message: 'Problems parsing JSON' }); return; }
      if (body.value.state === 'closed' || body.value.state === 'open') rec.state = body.value.state;
      if (typeof body.value.title === 'string') rec.title = body.value.title;
      if (typeof body.value.body === 'string') rec.body = body.value.body;
      rec.ver += 1;
      sendJson(res, 200, issueJson(repo, rec)); return;
    }
    return notFound();
  }
  if (rest.length === 3 && rest[2] === 'comments') {
    if (!rec) return notFound();
    if (method === 'POST') {
      const body = parseJsonBody(raw); if (!body.ok) { sendJson(res, 400, { message: 'Problems parsing JSON' }); return; }
      const comment = { id: nextIssueId++, body: String(body.value.body ?? '') };
      rec.comments.push(comment);
      rec.ver += 1;
      sendJson(res, 201, { id: comment.id, body: comment.body, user: { login: VIEWER.login, id: VIEWER.id } });
      return;
    }
    if (method === 'GET') { sendJson(res, 200, rec.comments.map((c) => ({ id: c.id, body: c.body, user: { login: VIEWER.login, id: VIEWER.id } }))); return; }
    return notFound();
  }
  return notFound();
}

function routePulls(res: ServerResponse, repo: Repo, rest: string[], url: URL, method: string, raw: Buffer, host: string): void {
  const notFound = (): void => sendJson(res, 404, { message: 'Not Found' });
  if (rest.length === 1) {
    if (method === 'POST') {
      const body = parseJsonBody(raw); if (!body.ok) { sendJson(res, 400, { message: 'Problems parsing JSON' }); return; }
      const head = String(body.value.head ?? '').replace(new RegExp(`^${VIEWER.login}:`), '');
      const base = String(body.value.base ?? repo.defaultBranch);
      if (!branchTip(repo, head)) { sendJson(res, 422, { message: 'Validation Failed: head branch does not exist' }); return; }
      const rec: IssueRec = {
        kind: 'pull', number: repo.nextNumber++, id: nextIssueId++,
        title: String(body.value.title ?? ''), body: String(body.value.body ?? ''),
        state: 'open', labels: [], comments: [], createdSeq: ++seq, ver: 0,
        headRef: head, baseRef: base, merged: false, mergeCommitSha: null,
      };
      repo.issues.push(rec);
      sendJson(res, 201, pullJson(repo, rec));
      return;
    }
    if (method === 'GET') {
      const state = url.searchParams.get('state') ?? 'open';
      let list = repo.issues.filter((i) => i.kind === 'pull');
      if (state === 'open') list = list.filter((i) => !i.merged && i.state === 'open');
      else if (state === 'closed') list = list.filter((i) => i.merged || i.state === 'closed');
      const { page, link } = paginate(url, list.map((i) => pullJson(repo, i)), host);
      sendJson(res, 200, page, link ? { link } : {});
      return;
    }
    return notFound();
  }
  const num = Number(rest[1]);
  const rec = repo.issues.find((i) => i.kind === 'pull' && i.number === num);
  if (rest.length === 2 && method === 'GET') {
    if (!rec) return notFound();
    sendJson(res, 200, pullJson(repo, rec)); return;
  }
  if (rest.length === 3 && rest[2] === 'files' && method === 'GET') {
    if (!rec) return notFound();
    sendJson(res, 200, pullFiles(repo, rec)); return;
  }
  if (rest.length === 3 && rest[2] === 'merge' && method === 'PUT') {
    if (!rec) return notFound();
    if (rec.merged) { sendJson(res, 405, { message: 'Pull Request is not mergeable' }); return; }
    const headSha = rec.headRef ? branchTip(repo, rec.headRef) : undefined;
    const baseSha = rec.baseRef ? branchTip(repo, rec.baseRef) : undefined;
    if (!headSha || !baseSha) { sendJson(res, 405, { message: 'Pull Request is not mergeable' }); return; }
    const mergedTree = mergeTrees(repo, baseSha, headSha);
    if (mergedTree === null) { sendJson(res, 405, { message: 'Pull Request is not mergeable: conflicts' }); return; }
    const commitSha = makeCommit(repo, mergedTree, [baseSha], `${rec.title} (#${rec.number})`);
    repo.refs.set(`refs/heads/${rec.baseRef}`, commitSha);
    rec.merged = true; rec.state = 'closed'; rec.mergeCommitSha = commitSha; rec.mergedTs = nextTs();
    sendJson(res, 200, { sha: commitSha, merged: true, message: 'Pull Request successfully merged' });
    return;
  }
  return notFound();
}

// ---------------------------------------------------------------- git smart HTTP (V2)
import { pktLine, PKT_FLUSH, PKT_DELIM, decodePkts, buildPack, parsePack, ZERO } from './git.ts';
import type { ServerResponse as SrvRes } from 'node:http';

function peelToCommit(repo: Repo, sha: string): string {
  let cur = sha;
  for (let i = 0; i < 10; i++) {
    const obj = repo.objects.get(cur);
    if (!obj) return cur;
    if (obj.type === 'commit') return cur;
    if (obj.type === 'tag') {
      const m = /^object ([0-9a-f]{40})$/m.exec(obj.data.toString('utf8'));
      if (m) { cur = m[1]; continue; }
    }
    return cur;
  }
  return cur;
}
function sortedRefEntries(repo: Repo): [string, string][] {
  return [...repo.refs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1));
}
function objectClosure(repo: Repo, roots: string[]): Set<string> {
  const out = new Set<string>(); const stack = [...roots];
  while (stack.length) {
    const sha = stack.pop()!;
    if (out.has(sha)) continue;
    const obj = repo.objects.get(sha);
    if (!obj) continue;
    out.add(sha);
    if (obj.type === 'commit') { const c = parseCommit(obj.data); stack.push(c.tree, ...c.parents); }
    else if (obj.type === 'tree') for (const e of parseTree(obj.data)) stack.push(e.sha);
    else if (obj.type === 'tag') { const m = /^object ([0-9a-f]{40})$/m.exec(obj.data.toString('utf8')); if (m) stack.push(m[1]); }
  }
  return out;
}

function advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack', v2: boolean): Buffer {
  if (service === 'git-upload-pack' && v2) {
    const lines = ['version 2\n', 'agent=forge/1.0\n', 'object-format=sha1\n', 'ls-refs=unborn\n', 'fetch=shallow wait-for-done\n'];
    return Buffer.concat([...lines.map((l) => pktLine(l)), PKT_FLUSH]);
  }
  const caps = service === 'git-upload-pack'
    ? `multi_ack_detailed no-done side-band-64k ofs-delta shallow symref=HEAD:refs/heads/${repo.defaultBranch} agent=forge/1.0`
    : 'report-status delete-refs side-band-64k ofs-delta agent=forge/1.0';
  const parts: Buffer[] = [pktLine(`# service=${service}\n`), PKT_FLUSH];
  const refs = sortedRefEntries(repo);
  const headRef = `refs/heads/${repo.defaultBranch}`;
  const headTip = repo.refs.get(headRef);
  if (refs.length === 0) {
    parts.push(pktLine(`${ZERO} capabilities^{}\0${caps}\n`));
  } else {
    let first = true;
    const emit = (oid: string, name: string): void => {
      if (first) { parts.push(pktLine(`${oid} ${name}\0${caps}\n`)); first = false; }
      else parts.push(pktLine(`${oid} ${name}\n`));
    };
    if (headTip) emit(headTip, 'HEAD');
    for (const [name, oid] of refs) {
      emit(oid, name);
      const obj = repo.objects.get(oid);
      if (obj && obj.type === 'tag') emit(peelToCommit(repo, oid), `${name}^{}`);
    }
    if (!headTip && first) parts.push(pktLine(`${ZERO} capabilities^{}\0${caps}\n`));
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

function lsRefs(repo: Repo, body: Buffer): Buffer {
  const { packets } = decodePkts(body);
  let peel = false; let symrefs = false; let unborn = false;
  const prefixes: string[] = [];
  let pastDelim = false;
  for (const p of packets) {
    if (p.kind === 'delim') { pastDelim = true; continue; }
    if (p.kind === 'flush') break;
    if (p.kind !== 'data' || !pastDelim) continue;
    const line = p.payload.toString('utf8').replace(/\n$/, '');
    if (line === 'peel') peel = true;
    else if (line === 'symrefs') symrefs = true;
    else if (line === 'unborn') unborn = true;
    else if (line.startsWith('ref-prefix ')) prefixes.push(line.slice('ref-prefix '.length));
  }
  const keep = (name: string): boolean => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));
  const out: Buffer[] = [];
  const headRef = `refs/heads/${repo.defaultBranch}`;
  const headTip = repo.refs.get(headRef);
  if (keep('HEAD')) {
    if (headTip) out.push(pktLine(`${headTip} HEAD${symrefs ? ` symref-target:${headRef}` : ''}\n`));
    else if (unborn) out.push(pktLine(`unborn HEAD${symrefs ? ` symref-target:${headRef}` : ''}\n`));
  }
  for (const [name, oid] of sortedRefEntries(repo)) {
    if (!keep(name)) continue;
    let line = `${oid} ${name}`;
    if (peel) {
      const obj = repo.objects.get(oid);
      if (obj && obj.type === 'tag') line += ` peeled:${peelToCommit(repo, oid)}`;
    }
    out.push(pktLine(`${line}\n`));
  }
  out.push(PKT_FLUSH);
  return Buffer.concat(out);
}

interface FetchPlan { pack: Buffer; shallow: string[]; unshallow: string[]; commonHave: string | undefined }
/** Object closure of `roots`, but a commit in `cut` contributes only itself + its tree (its parents are NOT had). */
function closureCutAt(repo: Repo, roots: string[], cut: Set<string>): Set<string> {
  const out = new Set<string>(); const stack = [...roots];
  while (stack.length) {
    const sha = stack.pop()!;
    if (out.has(sha)) continue;
    const obj = repo.objects.get(sha);
    if (!obj) continue;
    out.add(sha);
    if (obj.type === 'commit') { const c = parseCommit(obj.data); stack.push(c.tree); if (!cut.has(sha)) stack.push(...c.parents); }
    else if (obj.type === 'tree') for (const e of parseTree(obj.data)) stack.push(e.sha);
    else if (obj.type === 'tag') { const m = /^object ([0-9a-f]{40})$/m.exec(obj.data.toString('utf8')); if (m) stack.push(m[1]); }
  }
  return out;
}
function planFetch(repo: Repo, wants: string[], haves: string[], boundaries: string[], depth: number | null): FetchPlan {
  const boundarySetPre = new Set(boundaries.filter((b) => repo.objects.has(b)));
  const hadClosure = closureCutAt(repo, haves.filter((h) => repo.objects.has(h)), boundarySetPre);
  const boundarySet = boundarySetPre;
  const hadObjects = new Set<string>(hadClosure);
  for (const b of boundarySet) for (const sha of objectClosure(repo, [b])) {
    // boundary commit's own generation is had, but NOT its parents' objects:
    const obj = repo.objects.get(b);
    hadObjects.add(b);
    if (obj && obj.type === 'commit') for (const sha of objectClosure(repo, [parseCommit(obj.data).tree])) hadObjects.add(sha);
  }
  const included = new Map<string, number>(); // commit sha -> generation
  const queue: { sha: string; gen: number }[] = [];
  const tagObjects: string[] = [];
  for (const w of wants) {
    const obj = repo.objects.get(w);
    if (!obj) continue;
    if (obj.type === 'tag') { tagObjects.push(w); const target = peelToCommit(repo, w); queue.push({ sha: target, gen: 0 }); }
    else if (obj.type === 'commit') queue.push({ sha: w, gen: 0 });
  }
  while (queue.length) {
    const { sha, gen } = queue.shift()!;
    if (included.has(sha) && included.get(sha)! <= gen) continue;
    if (gen > 0 && hadClosure.has(sha)) continue; // haves prune traversal, but a want root is always walked
    if (depth !== null && gen >= depth) continue;
    included.set(sha, gen);
    const obj = repo.objects.get(sha);
    if (!obj || obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) queue.push({ sha: parent, gen: gen + 1 });
  }
  const send = new Map<string, GitObj>();
  const addObj = (sha: string): void => {
    if (hadObjects.has(sha) || send.has(sha)) return;
    const obj = repo.objects.get(sha);
    if (obj) send.set(sha, obj);
  };
  for (const t of tagObjects) addObj(t);
  const shallow: string[] = []; const unshallow: string[] = [];
  for (const [sha] of included) {
    const obj = repo.objects.get(sha);
    if (!obj || obj.type !== 'commit') continue;
    const info = parseCommit(obj.data);
    if (!boundarySet.has(sha)) {
      addObj(sha);
      for (const o of objectClosure(repo, [info.tree])) addObj(o);
    }
    const parentsInRepo = info.parents.filter((p) => repo.objects.has(p));
    const parentsIncluded = parentsInRepo.filter((p) => included.has(p));
    if (parentsInRepo.length > 0 && parentsIncluded.length < parentsInRepo.length) shallow.push(sha);
    else if (boundarySet.has(sha)) unshallow.push(sha);
  }
  const pack = buildPack([...send.values()].map((o) => ({ type: o.type, data: o.data })));
  const commonHave = haves.find((h) => repo.objects.has(h));
  return { pack, shallow, unshallow, commonHave };
}
function sidebandChunks(pack: Buffer): Buffer[] {
  const out: Buffer[] = [];
  const CHUNK = 65515;
  for (let i = 0; i < pack.length; i += CHUNK) {
    out.push(pktLine(Buffer.concat([Buffer.from([1]), pack.subarray(i, i + CHUNK)])));
  }
  if (pack.length === 0) out.push(pktLine(Buffer.concat([Buffer.from([1]), Buffer.alloc(0)])));
  return out;
}
function fetchV2(repo: Repo, body: Buffer): Buffer {
  const { packets } = decodePkts(body);
  const wants: string[] = []; const haves: string[] = []; const boundaries: string[] = [];
  let depth: number | null = null;
  for (const p of packets) {
    if (p.kind !== 'data') continue;
    const line = p.payload.toString('utf8').replace(/\n$/, '');
    if (line.startsWith('want ')) wants.push(line.slice(5).trim());
    else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) boundaries.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) depth = Number(line.slice(7).trim());
  }
  const fetchKey = `v2|${repo.objects.size}|${wants.join(',')}|${haves.join(',')}|${boundaries.join(',')}|${depth}`;
  if (repo.fetchCacheKey === fetchKey && repo.fetchCacheBuf !== undefined) return repo.fetchCacheBuf;
  const plan = planFetch(repo, wants, haves, boundaries, depth);
  const out: Buffer[] = [];
  if (haves.length) {
    out.push(pktLine('acknowledgments\n'));
    if (plan.commonHave) { out.push(pktLine(`ACK ${plan.commonHave}\n`)); out.push(pktLine('ready\n')); }
    else out.push(pktLine('NAK\n'));
    out.push(PKT_DELIM);
  }
  if (depth !== null || boundaries.length) {
    out.push(pktLine('shallow-info\n'));
    for (const s of plan.shallow) out.push(pktLine(`shallow ${s}\n`));
    for (const u of plan.unshallow) out.push(pktLine(`unshallow ${u}\n`));
    out.push(PKT_DELIM);
  }
  out.push(pktLine('packfile\n'));
  out.push(...sidebandChunks(plan.pack));
  out.push(PKT_FLUSH);
  const fetchBuf = Buffer.concat(out);
  repo.fetchCacheKey = fetchKey; repo.fetchCacheBuf = fetchBuf;
  return fetchBuf;
}
function fetchV0(repo: Repo, body: Buffer): Buffer {
  const { packets } = decodePkts(body);
  const wants: string[] = []; const haves: string[] = []; const boundaries: string[] = [];
  let depth: number | null = null;
  for (const p of packets) {
    if (p.kind !== 'data') continue;
    let line = p.payload.toString('utf8').replace(/\n$/, '');
    const nul = line.indexOf('\0'); if (nul >= 0) line = line.slice(0, nul);
    if (line.startsWith('want ')) wants.push(line.slice(5).trim().split(' ')[0]);
    else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) boundaries.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) depth = Number(line.slice(7).trim());
  }
  const fetchKey = `v0|${repo.objects.size}|${wants.join(',')}|${haves.join(',')}|${boundaries.join(',')}|${depth}`;
  if (repo.fetchCacheKey === fetchKey && repo.fetchCacheBuf !== undefined) return repo.fetchCacheBuf;
  const plan = planFetch(repo, wants, haves, boundaries, depth);
  const out: Buffer[] = [];
  for (const s of plan.shallow) out.push(pktLine(`shallow ${s}\n`));
  for (const u of plan.unshallow) out.push(pktLine(`unshallow ${u}\n`));
  out.push(pktLine(plan.commonHave ? `ACK ${plan.commonHave}\n` : 'NAK\n'));
  out.push(...sidebandChunks(plan.pack));
  out.push(PKT_FLUSH);
  const fetchBuf = Buffer.concat(out);
  repo.fetchCacheKey = fetchKey; repo.fetchCacheBuf = fetchBuf;
  return fetchBuf;
}
function receivePack(repo: Repo, body: Buffer): Buffer {
  const { packets, rest } = decodePkts(body);
  interface Cmd { oldId: string; newId: string; ref: string }
  const cmds: Cmd[] = [];
  for (const p of packets) {
    if (p.kind === 'flush') break;
    if (p.kind !== 'data') continue;
    let line = p.payload.toString('utf8').replace(/\n$/, '');
    const nul = line.indexOf('\0'); if (nul >= 0) line = line.slice(0, nul);
    const parts = line.split(' ');
    if (parts.length >= 3) cmds.push({ oldId: parts[0], newId: parts[1], ref: parts[2] });
  }
  const packBuf = body.subarray(rest);
  let unpackOk = true;
  let newObjects = new Map<string, GitObj>();
  if (packBuf.length > 0) {
    try {
      const parsed = parsePack(packBuf, (sha) => repo.objects.get(sha));
      newObjects = parsed.objects;
    } catch { unpackOk = false; }
  }
  const lookup = (sha: string): GitObj | undefined => newObjects.get(sha) ?? repo.objects.get(sha);
  const descends = (from: string, to: string): boolean => {
    const seen = new Set<string>(); const stack = [to];
    while (stack.length) {
      const cur = stack.pop()!;
      if (cur === from) return true;
      if (seen.has(cur)) continue; seen.add(cur);
      const obj = lookup(cur);
      if (obj && obj.type === 'commit') stack.push(...parseCommit(obj.data).parents);
    }
    return false;
  };
  const results: { ref: string; ok: boolean; reason?: string }[] = [];
  if (unpackOk) {
    for (const [sha, obj] of newObjects) if (!repo.objects.has(sha)) repo.objects.set(sha, obj);
    for (const cmd of cmds) {
      const current = repo.refs.get(cmd.ref);
      if (cmd.newId === ZERO) {
        if (current !== undefined) { repo.refs.delete(cmd.ref); results.push({ ref: cmd.ref, ok: true }); }
        else results.push({ ref: cmd.ref, ok: false, reason: 'reference does not exist' });
        continue;
      }
      if (!lookup(cmd.newId)) { results.push({ ref: cmd.ref, ok: false, reason: 'missing object' }); continue; }
      if (cmd.oldId === ZERO) {
        repo.refs.set(cmd.ref, cmd.newId);
        results.push({ ref: cmd.ref, ok: true });
        continue;
      }
      if (current !== cmd.oldId) { results.push({ ref: cmd.ref, ok: false, reason: 'stale ref' }); continue; }
      const newObj = lookup(cmd.newId);
      if (newObj && newObj.type === 'commit' && !descends(cmd.oldId, cmd.newId)) {
        results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
        continue;
      }
      repo.refs.set(cmd.ref, cmd.newId);
      results.push({ ref: cmd.ref, ok: true });
    }
  }
  const reportLines: Buffer[] = [pktLine(unpackOk ? 'unpack ok\n' : 'unpack error\n')];
  if (unpackOk) for (const r of results) reportLines.push(pktLine(r.ok ? `ok ${r.ref}\n` : `ng ${r.ref} ${r.reason}\n`));
  reportLines.push(PKT_FLUSH);
  const report = Buffer.concat(reportLines);
  const out: Buffer[] = [];
  const CHUNK = 65515;
  for (let i = 0; i < report.length; i += CHUNK) out.push(pktLine(Buffer.concat([Buffer.from([1]), report.subarray(i, i + CHUNK)])));
  out.push(PKT_FLUSH);
  return Buffer.concat(out);
}

function sendBuffer(res: SrvRes, status: number, buf: Buffer, contentType: string): void {
  requestCounter += 1;
  res.writeHead(status, { 'content-type': contentType, 'content-length': buf.length, 'x-github-request-id': `req-${requestCounter.toString(16).padStart(8, '0')}` });
  res.end(buf);
}

async function routeGit(req: IncomingMessage, res: ServerResponse, url: URL, method: string, raw: Buffer): Promise<boolean> {
  const m = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(url.pathname);
  if (!m) return false;
  const owner = decodeURIComponent(m[1]); const name = decodeURIComponent(m[2]);
  if (!authed(req)) { sendJson(res, 401, { message: 'Bad credentials' }); return true; }
  const repo = findRepo(owner, name);
  if (!repo) { sendJson(res, 404, { message: 'Not Found' }); return true; }
  const service = url.searchParams.get('service') ?? '';
  if (method === 'GET' && m[3] === 'info/refs') {
    if (service !== 'git-upload-pack' && service !== 'git-receive-pack') { sendJson(res, 404, { message: 'Not Found' }); return true; }
    const v2 = (req.headers['git-protocol'] ?? '').includes('version=2');
    sendBuffer(res, 200, advertisement(repo, service as 'git-upload-pack' | 'git-receive-pack', v2), `application/x-${service}-advertisement`);
    return true;
  }
  if (method === 'POST' && m[3] === 'git-upload-pack') {
    const firstLine = decodePkts(raw).packets.find((p) => p.kind === 'data');
    const text = firstLine && firstLine.kind === 'data' ? firstLine.payload.toString('utf8') : '';
    let buf: Buffer;
    if (text.startsWith('command=ls-refs')) buf = lsRefs(repo, raw);
    else if (text.startsWith('command=fetch')) buf = fetchV2(repo, raw);
    else buf = fetchV0(repo, raw);
    sendBuffer(res, 200, buf, 'application/x-git-upload-pack-result');
    return true;
  }
  if (method === 'POST' && m[3] === 'git-receive-pack') {
    sendBuffer(res, 200, receivePack(repo, raw), 'application/x-git-receive-pack-result');
    return true;
  }
  sendJson(res, 404, { message: 'Not Found' });
  return true;
}

// ---------------------------------------------------------------- GraphQL (V3): parser + executor + introspection
type GqlValue =
  | { k: 'var'; name: string } | { k: 'str'; v: string } | { k: 'num'; v: number }
  | { k: 'bool'; v: boolean } | { k: 'null' } | { k: 'list'; items: GqlValue[] }
  | { k: 'obj'; fields: { name: string; value: GqlValue }[] };
interface GqlField { name: string; alias?: string; args: { name: string; value: GqlValue }[]; sels?: GqlSel[]; dirs: { name: string; args: { name: string; value: GqlValue }[] }[] }
type GqlSel = GqlField | { spread: string; dirs: GqlField['dirs'] } | { inline: string | null; sels: GqlSel[]; dirs: GqlField['dirs'] };
interface GqlOp { op: 'query' | 'mutation'; name?: string; varDefs: { name: string; def?: GqlValue }[]; sels: GqlSel[] }
interface GqlDoc { ops: GqlOp[]; frags: Map<string, { type: string; sels: GqlSel[] }> }

function gqlParse(src: string): GqlDoc {
  let i = 0;
  const err = (m: string): Error => new Error(`GraphQL parse error: ${m} at ${i}`);
  const skip = (): void => { for (;;) { while (i < src.length && /[\s,]/.test(src[i])) i++; if (src[i] === '#') { while (i < src.length && src[i] !== '\n') i++; continue; } break; } };
  const peek = (): string => { skip(); return src[i] ?? ''; };
  const expect = (c: string): void => { skip(); if (src[i] !== c) throw err(`expected ${c}`); i++; };
  const name = (): string => { skip(); const m = /^[_A-Za-z][_0-9A-Za-z]*/.exec(src.slice(i)); if (!m) throw err('expected name'); i += m[0].length; return m[0]; };
  const str = (): string => {
    skip();
    if (src.startsWith('"""', i)) { const end = src.indexOf('"""', i + 3); const v = src.slice(i + 3, end); i = end + 3; return v; }
    if (src[i] !== '"') throw err('expected string');
    let j = i + 1; let out = '';
    while (src[j] !== '"') { if (src[j] === '\\') { const e = src[j + 1]; out += e === 'n' ? '\n' : e === 't' ? '\t' : e === 'r' ? '\r' : e === 'u' ? String.fromCharCode(parseInt(src.slice(j + 2, j + 6), 16)) : e; j += e === 'u' ? 6 : 2; } else { out += src[j]; j++; } }
    i = j + 1; return out;
  };
  const value = (): GqlValue => {
    skip(); const c = src[i];
    if (c === '$') { i++; return { k: 'var', name: name() }; }
    if (c === '"') return { k: 'str', v: str() };
    if (c === '[') { i++; const items: GqlValue[] = []; while (peek() !== ']') items.push(value()); expect(']'); return { k: 'list', items }; }
    if (c === '{') { i++; const fields: { name: string; value: GqlValue }[] = []; while (peek() !== '}') { const n = name(); expect(':'); fields.push({ name: n, value: value() }); } expect('}'); return { k: 'obj', fields }; }
    if (c === '-' || /[0-9]/.test(c)) { const m = /^-?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?/.exec(src.slice(i))!; i += m[0].length; return { k: 'num', v: Number(m[0]) }; }
    const n = name();
    if (n === 'true') return { k: 'bool', v: true };
    if (n === 'false') return { k: 'bool', v: false };
    if (n === 'null') return { k: 'null' };
    return { k: 'str', v: n }; // enum literal
  };
  const typeRefStr = (): string => {
    skip();
    let t: string;
    if (src[i] === '[') { i++; const inner = typeRefStr(); expect(']'); t = `[${inner}]`; } else t = name();
    skip(); if (src[i] === '!') { i++; t += '!'; }
    return t;
  };
  const args = (): GqlField['args'] => {
    const out: GqlField['args'] = [];
    if (peek() !== '(') return out;
    i++;
    while (peek() !== ')') { const n = name(); expect(':'); out.push({ name: n, value: value() }); }
    expect(')');
    return out;
  };
  const dirs = (): GqlField['dirs'] => {
    const out: GqlField['dirs'] = [];
    while (peek() === '@') { i++; const n = name(); out.push({ name: n, args: args() }); }
    return out;
  };
  const selSet = (): GqlSel[] => {
    expect('{');
    const out: GqlSel[] = [];
    while (peek() !== '}') {
      if (src.startsWith('...', i)) {
        i += 3; skip();
        if (/^on\b/.test(src.slice(i)) && !/^[_0-9A-Za-z]/.test(src.slice(i + 2, i + 3))) { i += 2; const t = name(); const d = dirs(); out.push({ inline: t, sels: selSet(), dirs: d }); }
        else { const n = name(); const d = dirs(); out.push({ spread: n, dirs: d }); }
        continue;
      }
      let n = name(); let alias: string | undefined;
      if (peek() === ':') { i++; alias = n; n = name(); }
      const a = args(); const d = dirs();
      const sels = peek() === '{' ? selSet() : undefined;
      out.push({ name: n, alias, args: a, sels, dirs: d });
    }
    expect('}');
    return out;
  };
  const doc: GqlDoc = { ops: [], frags: new Map() };
  for (;;) {
    skip(); if (i >= src.length) break;
    if (peek() === '{') { doc.ops.push({ op: 'query', varDefs: [], sels: selSet() }); continue; }
    const kw = name();
    if (kw === 'query' || kw === 'mutation' || kw === 'subscription') {
      let opName: string | undefined;
      if (/[_A-Za-z]/.test(peek())) opName = name();
      const varDefs: GqlOp['varDefs'] = [];
      if (peek() === '(') {
        i++;
        while (peek() !== ')') {
          expect('$'); const vn = name(); expect(':'); typeRefStr();
          let def: GqlValue | undefined;
          skip(); if (src[i] === '=') { i++; def = value(); }
          varDefs.push({ name: vn, def });
          dirs();
        }
        expect(')');
      }
      dirs();
      doc.ops.push({ op: kw === 'mutation' ? 'mutation' : 'query', name: opName, varDefs, sels: selSet() });
    } else if (kw === 'fragment') {
      const fn = name(); skip(); const on = name(); if (on !== 'on') throw err('expected on');
      const t = name(); dirs();
      doc.frags.set(fn, { type: t, sels: selSet() });
    } else throw err(`unexpected ${kw}`);
  }
  return doc;
}

function gqlEval(v: GqlValue, vars: Record<string, unknown>): unknown {
  switch (v.k) {
    case 'var': return vars[v.name];
    case 'str': case 'num': case 'bool': return v.v;
    case 'null': return null;
    case 'list': return v.items.map((x) => gqlEval(x, vars));
    case 'obj': { const o: Record<string, unknown> = {}; for (const f of v.fields) o[f.name] = gqlEval(f.value, vars); return o; }
  }
}

type TypeRef =
  | { kind: 'NAMED'; name: string }
  | { kind: 'LIST'; of: TypeRef }
  | { kind: 'NON_NULL'; of: TypeRef };
function parseTypeRef(s: string): TypeRef {
  const t = s.trim();
  if (t.endsWith('!')) return { kind: 'NON_NULL', of: parseTypeRef(t.slice(0, -1)) };
  if (t.startsWith('[') && t.endsWith(']')) return { kind: 'LIST', of: parseTypeRef(t.slice(1, -1)) };
  return { kind: 'NAMED', name: t };
}
function namedOf(ref: TypeRef): string { return ref.kind === 'NAMED' ? ref.name : namedOf(ref.of); }

interface GqlFieldDef { type: string; args?: Record<string, string>; resolve: (src: any, args: Record<string, any>, ctx: GqlCtx) => unknown }
interface GqlTypeDef {
  kind: 'OBJECT' | 'INTERFACE' | 'UNION' | 'ENUM' | 'INPUT_OBJECT' | 'SCALAR';
  fields?: Record<string, GqlFieldDef>;
  possibleTypes?: string[];
  enumValues?: string[];
  inputFields?: Record<string, string>;
  resolveType?: (src: any) => string;
}
interface GqlCtx { vars: Record<string, unknown>; errors: { message: string; path?: (string | number)[] }[]; doc: GqlDoc }
type GqlSchema = Map<string, GqlTypeDef>;
const SCALARS = new Set(['String', 'Int', 'Float', 'Boolean', 'ID', 'DateTime', 'URI', 'GitObjectID', 'Base64String', 'HTML', 'GitTimestamp', 'PreciseDateTime', 'X509Certificate', 'GitSSHRemote', 'GitRefname']);

function gqlDirOk(dirs: GqlField['dirs'], vars: Record<string, unknown>): boolean {
  for (const d of dirs) {
    const ifArg = d.args.find((a) => a.name === 'if');
    const val = ifArg ? gqlEval(ifArg.value, vars) : undefined;
    if (d.name === 'skip' && val === true) return false;
    if (d.name === 'include' && val === false) return false;
  }
  return true;
}

function gqlExecSet(schema: GqlSchema, typeName: string, src: any, sels: GqlSel[], ctx: GqlCtx, path: (string | number)[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const runtimeType = src && typeof src === 'object' && typeof src.__gqlType === 'string' ? src.__gqlType : typeName;
  const typeDef = schema.get(typeName);
  const runField = (f: GqlField): void => {
    const key = f.alias ?? f.name;
    const fpath = [...path, key];
    if (!gqlDirOk(f.dirs, ctx.vars)) return;
    if (f.name === '__typename') { out[key] = runtimeType; return; }
    if (f.name === '__schema' && typeName === 'Query') { out[key] = complete(schema, parseTypeRef('__Schema'), { __gqlType: '__Schema', schema }, f, ctx, fpath); return; }
    if (f.name === '__type' && typeName === 'Query') {
      const a = evalArgs(f, ctx);
      const t = typeof a.name === 'string' ? schema.get(a.name) : undefined;
      out[key] = t ? complete(schema, parseTypeRef('__Type'), { __gqlType: '__Type', typeName: a.name }, f, ctx, fpath) : null;
      return;
    }
    const def = typeDef?.fields?.[f.name] ?? (runtimeType !== typeName ? schema.get(runtimeType)?.fields?.[f.name] : undefined);
    if (!def) {
      ctx.errors.push({ message: `Field '${f.name}' doesn't exist on type '${typeName}'`, path: fpath });
      out[key] = null;
      return;
    }
    let val: unknown;
    try { val = def.resolve(src, evalArgs(f, ctx), ctx); } catch (e) { ctx.errors.push({ message: (e as Error).message, path: fpath }); val = null; }
    out[key] = complete(schema, parseTypeRef(def.type), val, f, ctx, fpath);
  };
  const walk = (list: GqlSel[], currentType: string): void => {
    for (const sel of list) {
      if ('spread' in sel) {
        const frag = ctx.doc.frags.get(sel.spread);
        if (frag && (frag.type === currentType || frag.type === runtimeType || schema.get(frag.type)?.kind !== 'OBJECT')) walk(frag.sels, frag.type);
        continue;
      }
      if ('inline' in sel) {
        if (sel.inline === null || sel.inline === currentType || sel.inline === runtimeType) walk(sel.sels, sel.inline ?? currentType);
        continue;
      }
      runField(sel);
    }
  };
  walk(sels, typeName);
  return out;
}
function evalArgs(f: GqlField, ctx: GqlCtx): Record<string, any> {
  const out: Record<string, any> = {};
  for (const a of f.args) out[a.name] = gqlEval(a.value, ctx.vars);
  return out;
}
function complete(schema: GqlSchema, ref: TypeRef, val: unknown, f: GqlField, ctx: GqlCtx, path: (string | number)[]): unknown {
  if (ref.kind === 'NON_NULL') {
    const inner = complete(schema, ref.of, val, f, ctx, path);
    if (inner === null || inner === undefined) ctx.errors.push({ message: `Cannot return null for non-nullable field`, path });
    return inner ?? null;
  }
  if (val === null || val === undefined) return null;
  if (ref.kind === 'LIST') {
    const arr = Array.isArray(val) ? val : [val];
    return arr.map((item, idx) => complete(schema, ref.of, item, f, ctx, [...path, idx]));
  }
  const name = ref.name;
  if (SCALARS.has(name) || !schema.has(name)) {
    if (name === 'ID') return String(val);
    if (name === 'Int') return Math.trunc(Number(val));
    if (name === 'Boolean') return Boolean(val);
    return val;
  }
  const def = schema.get(name)!;
  if (def.kind === 'ENUM') return val;
  if (!f.sels) return val;
  let runtime = name;
  const src = val as any;
  if (def.kind === 'INTERFACE' || def.kind === 'UNION') runtime = def.resolveType ? def.resolveType(src) : src.__gqlType ?? name;
  else if (src && typeof src.__gqlType === 'string') runtime = src.__gqlType;
  return gqlExecSet(schema, runtime, src, f.sels, ctx, path);
}

function gqlRun(schema: GqlSchema, query: string, variables: Record<string, unknown>, operationName?: string): { data?: unknown; errors?: { message: string; path?: (string | number)[] }[] } {
  let doc: GqlDoc;
  try { doc = gqlParse(query); } catch (e) { return { errors: [{ message: (e as Error).message }] }; }
  const op = (operationName ? doc.ops.find((o) => o.name === operationName) : undefined) ?? doc.ops[0];
  if (!op) return { errors: [{ message: 'no operation found' }] };
  const vars: Record<string, unknown> = { ...variables };
  for (const vd of op.varDefs) if (vars[vd.name] === undefined && vd.def) vars[vd.name] = gqlEval(vd.def, vars);
  const ctx: GqlCtx = { vars, errors: [], doc };
  const rootType = op.op === 'mutation' ? 'Mutation' : 'Query';
  const data = gqlExecSet(schema, rootType, { __gqlType: rootType }, op.sels, ctx, []);
  const out: { data?: unknown; errors?: { message: string; path?: (string | number)[] }[] } = { data };
  if (ctx.errors.length) out.errors = ctx.errors;
  return out;
}

// introspection types, generated from the schema itself
function buildIntrospection(schema: GqlSchema): void {
  const typeWrapper = (name: string): any => ({ __gqlType: '__Type', typeName: name });
  const refToWrapper = (ref: TypeRef): any => {
    if (ref.kind === 'NAMED') return typeWrapper(ref.name);
    return { __gqlType: '__Type', wrapper: ref };
  };
  schema.set('__Type', {
    kind: 'OBJECT',
    fields: {
      name: { type: 'String', resolve: (s) => (s.wrapper ? null : s.typeName) },
      kind: {
        type: '__TypeKind!', resolve: (s) => {
          if (s.wrapper) return s.wrapper.kind;
          if (SCALARS.has(s.typeName)) return 'SCALAR';
          return schema.get(s.typeName)?.kind ?? 'OBJECT';
        },
      },
      description: { type: 'String', resolve: () => null },
      fields: {
        type: '[__Field]', args: { includeDeprecated: 'Boolean' },
        resolve: (s) => {
          const def = s.wrapper ? undefined : schema.get(s.typeName);
          if (!def?.fields) return null;
          return Object.entries(def.fields).map(([fname, fdef]) => ({ __gqlType: '__Field', name: fname, def: fdef }));
        },
      },
      inputFields: {
        type: '[__InputValue]',
        resolve: (s) => {
          const def = s.wrapper ? undefined : schema.get(s.typeName);
          if (!def?.inputFields) return null;
          return Object.entries(def.inputFields).map(([n, t]) => ({ __gqlType: '__InputValue', name: n, typeStr: t }));
        },
      },
      enumValues: {
        type: '[__EnumValue]', args: { includeDeprecated: 'Boolean' },
        resolve: (s) => {
          const def = s.wrapper ? undefined : schema.get(s.typeName);
          if (!def?.enumValues) return null;
          return def.enumValues.map((n) => ({ __gqlType: '__EnumValue', name: n }));
        },
      },
      possibleTypes: {
        type: '[__Type]',
        resolve: (s) => {
          const def = s.wrapper ? undefined : schema.get(s.typeName);
          if (!def?.possibleTypes) return null;
          return def.possibleTypes.map(typeWrapper);
        },
      },
      ofType: { type: '__Type', resolve: (s) => (s.wrapper ? refToWrapper(s.wrapper.of) : null) },
      interfaces: { type: '[__Type]', resolve: () => [] },
      specifiedByURL: { type: 'String', resolve: () => null },
    },
  });
  schema.set('__Field', {
    kind: 'OBJECT',
    fields: {
      name: { type: 'String!', resolve: (s) => s.name },
      description: { type: 'String', resolve: () => null },
      args: { type: '[__InputValue!]!', resolve: (s) => Object.entries(s.def.args ?? {}).map(([n, t]) => ({ __gqlType: '__InputValue', name: n, typeStr: t })) },
      type: { type: '__Type!', resolve: (s) => refToWrapper(parseTypeRef(s.def.type)) },
      isDeprecated: { type: 'Boolean!', resolve: () => false },
      deprecationReason: { type: 'String', resolve: () => null },
    },
  });
  schema.set('__InputValue', {
    kind: 'OBJECT',
    fields: {
      name: { type: 'String!', resolve: (s) => s.name },
      description: { type: 'String', resolve: () => null },
      type: { type: '__Type!', resolve: (s) => refToWrapper(parseTypeRef(s.typeStr)) },
      defaultValue: { type: 'String', resolve: () => null },
      isDeprecated: { type: 'Boolean!', resolve: () => false },
      deprecationReason: { type: 'String', resolve: () => null },
    },
  });
  schema.set('__EnumValue', {
    kind: 'OBJECT',
    fields: {
      name: { type: 'String!', resolve: (s) => s.name },
      description: { type: 'String', resolve: () => null },
      isDeprecated: { type: 'Boolean!', resolve: () => false },
      deprecationReason: { type: 'String', resolve: () => null },
    },
  });
  schema.set('__Schema', {
    kind: 'OBJECT',
    fields: {
      types: { type: '[__Type!]!', resolve: (s) => [...s.schema.keys()].filter((n) => !n.startsWith('__') || n === '__Type').map(typeWrapper) },
      queryType: { type: '__Type!', resolve: () => typeWrapper('Query') },
      mutationType: { type: '__Type', resolve: () => typeWrapper('Mutation') },
      subscriptionType: { type: '__Type', resolve: () => null },
      directives: { type: '[__Directive!]!', resolve: () => [] },
      description: { type: 'String', resolve: () => null },
    },
  });
  schema.set('__TypeKind', { kind: 'ENUM', enumValues: ['SCALAR', 'OBJECT', 'INTERFACE', 'UNION', 'ENUM', 'INPUT_OBJECT', 'LIST', 'NON_NULL'] });
}


// ---------------------------------------------------------------- GraphQL schema over the forge store
const b64e = (s: string): string => Buffer.from(s, 'utf8').toString('base64');
const b64d = (s: string): string => Buffer.from(s, 'base64').toString('utf8');
const repoNodeId = (repo: Repo): string => b64e(`${repo.id}:Repository`);
const issueNodeId = (repo: Repo, rec: IssueRec): string => b64e(`${repo.id}:${rec.number}:Issue`);
const prNodeId = (repo: Repo, rec: IssueRec): string => b64e(`${repo.id}:${rec.number}:PullRequest`);
const commentNodeId = (repo: Repo, rec: IssueRec, c: { id: number }): string => b64e(`${repo.id}:${rec.number}:${c.id}:IssueComment`);
const userNodeId = (): string => b64e(`${VIEWER.id}:User`);
const repoHtmlUrl = (repo: Repo): string => `http://forge/${VIEWER.login}/${repo.name}`;
const tsOf = (rec: { createdSeq: number }): string => isoOf(EPOCH + rec.createdSeq);
const wrapRepo = (repo: Repo): any => ({ __gqlType: 'Repository', repo });
const wrapIssue = (repo: Repo, rec: IssueRec): any => ({ __gqlType: 'Issue', repo, rec });
const wrapPr = (repo: Repo, rec: IssueRec): any => ({ __gqlType: 'PullRequest', repo, rec });
const wrapComment = (repo: Repo, rec: IssueRec, c: { id: number; body: string }): any => ({ __gqlType: 'IssueComment', repo, rec, comment: c });
const wrapUser = (): any => ({ __gqlType: 'User' });

interface GqlPage { edges: { node: any; cursor: string }[]; nodes: any[]; pageInfo: any }
function gqlPaginate(items: any[], args: Record<string, any>): { page: GqlPage; total: number } {
  const cursorOf = (idx: number): string => b64e(`cursor:${idx}`);
  const idxOf = (c: unknown): number => {
    if (typeof c !== 'string') return -1;
    const m = /^cursor:(\d+)$/.exec(b64d(c));
    return m ? Number(m[1]) : -1;
  };
  let start = 0; let end = items.length;
  const afterIdx = idxOf(args.after); if (afterIdx >= 0) start = afterIdx + 1;
  const beforeIdx = idxOf(args.before); if (beforeIdx >= 0) end = Math.min(end, beforeIdx);
  let slice = items.slice(start, end);
  let slicedStart = start;
  if (typeof args.first === 'number') slice = slice.slice(0, Math.max(0, args.first));
  if (typeof args.last === 'number') { const cut = Math.max(0, slice.length - args.last); slicedStart += cut; slice = slice.slice(cut); }
  const edges = slice.map((node, i) => ({ node, cursor: cursorOf(slicedStart + i) }));
  return {
    total: items.length,
    page: {
      edges, nodes: slice,
      pageInfo: {
        __gqlType: 'PageInfo',
        hasNextPage: slicedStart + slice.length < end,
        hasPreviousPage: slicedStart > start,
        startCursor: edges.length ? edges[0].cursor : null,
        endCursor: edges.length ? edges[edges.length - 1].cursor : null,
      },
    },
  };
}
function connFields(nodeType: string): Record<string, GqlFieldDef> {
  return {
    edges: { type: `[${nodeType}Edge]`, resolve: (s) => s.page.edges },
    nodes: { type: `[${nodeType}]`, resolve: (s) => s.page.nodes },
    pageInfo: { type: 'PageInfo!', resolve: (s) => s.page.pageInfo },
    totalCount: { type: 'Int!', resolve: (s) => s.total },
  };
}
const edgeFields = (nodeType: string): Record<string, GqlFieldDef> => ({
  node: { type: nodeType, resolve: (s) => s.node },
  cursor: { type: 'String!', resolve: (s) => s.cursor },
});
const CONN_ARGS = { first: 'Int', after: 'String', last: 'Int', before: 'String' };

function sortByOrder(items: IssueRec[], args: Record<string, any>): IssueRec[] {
  const ob = (args.orderBy ?? {}) as { field?: string; direction?: string };
  const dir = ob.direction === 'ASC' ? 1 : -1;
  return [...items].sort((a, b) => (a.createdSeq - b.createdSeq) * dir);
}
function repoById(id: number): Repo | undefined { return [...repos.values()].find((r) => r.id === id); }
function decodeNodeId(id: string): any {
  let raw: string;
  try { raw = b64d(id); } catch { return null; }
  let m = /^(\d+):Repository$/.exec(raw);
  if (m) { const repo = repoById(Number(m[1])); return repo ? wrapRepo(repo) : null; }
  m = /^(\d+):(\d+):Issue$/.exec(raw);
  if (m) { const repo = repoById(Number(m[1])); const rec = repo?.issues.find((x) => x.kind === 'issue' && x.number === Number(m[2])); return repo && rec ? wrapIssue(repo, rec) : null; }
  m = /^(\d+):(\d+):PullRequest$/.exec(raw);
  if (m) { const repo = repoById(Number(m[1])); const rec = repo?.issues.find((x) => x.kind === 'pull' && x.number === Number(m[2])); return repo && rec ? wrapPr(repo, rec) : null; }
  m = /^(\d+):(\d+):(\d+):IssueComment$/.exec(raw);
  if (m) { const repo = repoById(Number(m[1])); const rec = repo?.issues.find((x) => x.number === Number(m[2])); const c = rec?.comments.find((x) => x.id === Number(m[3])); return repo && rec && c ? wrapComment(repo, rec, c) : null; }
  m = /^(\d+):User$/.exec(raw);
  if (m && Number(m[1]) === VIEWER.id) return wrapUser();
  return null;
}
/** Same merge the REST PUT /pulls/:n/merge performs; returns the merge commit sha or null on conflict. */
function mergePullRec(repo: Repo, rec: IssueRec): string | null {
  if (rec.merged) return rec.mergeCommitSha ?? null;
  const headSha = rec.headRef ? branchTip(repo, rec.headRef) : undefined;
  const baseSha = rec.baseRef ? branchTip(repo, rec.baseRef) : undefined;
  if (!headSha || !baseSha) return null;
  const mergedTree = mergeTrees(repo, baseSha, headSha);
  if (mergedTree === null) return null;
  const commitSha = makeCommit(repo, mergedTree, [baseSha], `${rec.title} (#${rec.number})`);
  repo.refs.set(`refs/heads/${rec.baseRef}`, commitSha);
  rec.merged = true; rec.state = 'closed'; rec.mergeCommitSha = commitSha; rec.mergedTs = nextTs();
  return commitSha;
}
function wrapGitObject(repo: Repo, sha: string): any {
  const obj = repo.objects.get(sha);
  if (!obj) return { __gqlType: 'Blob', repo, sha };
  const t = obj.type === 'commit' ? 'Commit' : obj.type === 'tree' ? 'Tree' : 'Blob';
  return { __gqlType: t, repo, sha };
}
const ZERO_SHA = '0000000000000000000000000000000000000000';

function buildForgeSchema(): GqlSchema {
  const schema: GqlSchema = new Map();
  const S = (name: string, def: GqlTypeDef): void => { schema.set(name, def); };
  for (const sc of ['String', 'Int', 'Float', 'Boolean', 'ID', 'DateTime', 'URI', 'GitObjectID', 'Base64String', 'HTML', 'GitTimestamp', 'PreciseDateTime']) S(sc, { kind: 'SCALAR' });

  S('PageInfo', {
    kind: 'OBJECT',
    fields: {
      hasNextPage: { type: 'Boolean!', resolve: (s) => s.hasNextPage },
      hasPreviousPage: { type: 'Boolean!', resolve: (s) => s.hasPreviousPage },
      startCursor: { type: 'String', resolve: (s) => s.startCursor },
      endCursor: { type: 'String', resolve: (s) => s.endCursor },
    },
  });
  S('User', {
    kind: 'OBJECT',
    fields: {
      id: { type: 'ID!', resolve: () => userNodeId() },
      login: { type: 'String!', resolve: () => VIEWER.login },
      name: { type: 'String', resolve: () => VIEWER.name },
      email: { type: 'String', resolve: () => VIEWER.email },
      avatarUrl: { type: 'URI!', args: { size: 'Int' }, resolve: () => '' },
      url: { type: 'URI!', resolve: () => '' },
      databaseId: { type: 'Int', resolve: () => VIEWER.id },
      createdAt: { type: 'DateTime!', resolve: () => isoOf(EPOCH) },
      updatedAt: { type: 'DateTime!', resolve: () => isoOf(EPOCH) },
      isViewer: { type: 'Boolean!', resolve: () => true },
      repositories: {
        type: 'RepositoryConnection!', args: { ...CONN_ARGS, orderBy: 'RepositoryOrder' },
        resolve: (_s, args) => gqlPaginate([...repos.values()].map(wrapRepo), args),
      },
    },
  });
  S('RepositoryOwner', { kind: 'INTERFACE', possibleTypes: ['User'], resolveType: () => 'User', fields: { login: { type: 'String!', resolve: () => VIEWER.login }, id: { type: 'ID!', resolve: () => userNodeId() }, url: { type: 'URI!', resolve: () => '' } } });
  S('Node', { kind: 'INTERFACE', possibleTypes: ['Repository', 'Issue', 'PullRequest', 'User', 'IssueComment'], resolveType: (s) => s.__gqlType, fields: { id: { type: 'ID!', resolve: () => '' } } });
  S('Actor', { kind: 'INTERFACE', possibleTypes: ['User'], resolveType: () => 'User', fields: { login: { type: 'String!', resolve: () => VIEWER.login }, id: { type: 'ID!', resolve: () => userNodeId() }, url: { type: 'URI!', resolve: () => '' }, avatarUrl: { type: 'URI!', args: { size: 'Int' }, resolve: () => '' } } });
  S('RepositoryConnection', { kind: 'OBJECT', fields: connFields('Repository') });
  S('RepositoryEdge', { kind: 'OBJECT', fields: edgeFields('Repository') });

  S('Issue', {
    kind: 'OBJECT',
    fields: {
      id: { type: 'ID!', resolve: (s) => issueNodeId(s.repo, s.rec) },
      number: { type: 'Int!', resolve: (s) => s.rec.number },
      title: { type: 'String!', resolve: (s) => s.rec.title },
      body: { type: 'String', resolve: (s) => s.rec.body },
      bodyText: { type: 'String', resolve: (s) => s.rec.body },
      state: { type: 'IssueState!', resolve: (s) => s.rec.state.toUpperCase() },
      stateReason: { type: 'IssueStateReason', resolve: (s) => (s.rec.state === 'closed' ? 'COMPLETED' : null) },
      author: { type: 'Actor', resolve: () => wrapUser() },
      labels: {
        type: 'LabelConnection!', args: { ...CONN_ARGS },
        resolve: (s, args) => gqlPaginate(s.rec.labels.map((name: string) => ({ __gqlType: 'Label', repo: s.repo, name })), args),
      },
      comments: {
        type: 'IssueCommentConnection!', args: { ...CONN_ARGS },
        resolve: (s, args) => gqlPaginate(s.rec.comments.map((c: { id: number; body: string }) => wrapComment(s.repo, s.rec, c)), args),
      },
      createdAt: { type: 'DateTime!', resolve: (s) => tsOf(s.rec) },
      updatedAt: { type: 'DateTime!', resolve: (s) => tsOf(s.rec) },
      closedAt: { type: 'DateTime', resolve: () => null },
      url: { type: 'URI!', resolve: (s) => `${repoHtmlUrl(s.repo)}/issues/${s.rec.number}` },
      repository: { type: 'Repository!', resolve: (s) => wrapRepo(s.repo) },
      viewerDidAuthor: { type: 'Boolean!', resolve: () => true },
    },
  });
  S('Label', { kind: 'OBJECT', fields: { id: { type: 'ID!', resolve: (s) => b64e(`Label:${s.name}`) }, name: { type: 'String!', resolve: (s) => s.name }, color: { type: 'String!', resolve: (s) => s.repo.labels.get(s.name)?.color ?? 'ededed' }, description: { type: 'String', resolve: () => null } } });
  S('LabelConnection', { kind: 'OBJECT', fields: connFields('Label') });
  S('LabelEdge', { kind: 'OBJECT', fields: edgeFields('Label') });
  S('IssueConnection', { kind: 'OBJECT', fields: connFields('Issue') });
  S('IssueEdge', { kind: 'OBJECT', fields: edgeFields('Issue') });
  S('IssueComment', {
    kind: 'OBJECT',
    fields: {
      id: { type: 'ID!', resolve: (s) => commentNodeId(s.repo, s.rec, s.comment) },
      body: { type: 'String!', resolve: (s) => s.comment.body },
      bodyText: { type: 'String', resolve: (s) => s.comment.body },
      author: { type: 'Actor', resolve: () => wrapUser() },
      createdAt: { type: 'DateTime!', resolve: (s) => tsOf(s.rec) },
      updatedAt: { type: 'DateTime!', resolve: (s) => tsOf(s.rec) },
      url: { type: 'URI!', resolve: (s) => `${repoHtmlUrl(s.repo)}/issues/${s.rec.number}` },
    },
  });
  S('IssueCommentConnection', { kind: 'OBJECT', fields: connFields('IssueComment') });
  S('IssueCommentEdge', { kind: 'OBJECT', fields: edgeFields('IssueComment') });

  S('PullRequest', {
    kind: 'OBJECT',
    fields: {
      id: { type: 'ID!', resolve: (s) => prNodeId(s.repo, s.rec) },
      number: { type: 'Int!', resolve: (s) => s.rec.number },
      title: { type: 'String!', resolve: (s) => s.rec.title },
      body: { type: 'String', resolve: (s) => s.rec.body },
      bodyText: { type: 'String', resolve: (s) => s.rec.body },
      state: { type: 'PullRequestState!', resolve: (s) => (s.rec.merged ? 'MERGED' : s.rec.state.toUpperCase()) },
      merged: { type: 'Boolean!', resolve: (s) => !!s.rec.merged },
      mergedAt: { type: 'DateTime', resolve: (s) => (s.rec.merged ? isoOf(s.rec.mergedTs ?? EPOCH) : null) },
      closedAt: { type: 'DateTime', resolve: (s) => (s.rec.state === 'closed' ? tsOf(s.rec) : null) },
      isDraft: { type: 'Boolean!', resolve: () => false },
      author: { type: 'Actor', resolve: () => wrapUser() },
      baseRefName: { type: 'String!', resolve: (s) => s.rec.baseRef },
      headRefName: { type: 'String!', resolve: (s) => s.rec.headRef },
      baseRefOid: { type: 'GitObjectID!', resolve: (s) => (s.rec.baseRef ? branchTip(s.repo, s.rec.baseRef) : undefined) ?? ZERO_SHA },
      headRefOid: { type: 'GitObjectID!', resolve: (s) => (s.rec.headRef ? branchTip(s.repo, s.rec.headRef) : undefined) ?? ZERO_SHA },
      mergeable: {
        type: 'MergeableState!',
        resolve: (s) => {
          if (s.rec.merged || s.rec.state === 'closed') return 'UNKNOWN';
          const headSha = s.rec.headRef ? branchTip(s.repo, s.rec.headRef) : undefined;
          const baseSha = s.rec.baseRef ? branchTip(s.repo, s.rec.baseRef) : undefined;
          if (!headSha || !baseSha) return 'UNKNOWN';
          return mergeTrees(s.repo, baseSha, headSha) !== null ? 'MERGEABLE' : 'CONFLICTING';
        },
      },
      createdAt: { type: 'DateTime!', resolve: (s) => tsOf(s.rec) },
      updatedAt: { type: 'DateTime!', resolve: (s) => tsOf(s.rec) },
      url: { type: 'URI!', resolve: (s) => `${repoHtmlUrl(s.repo)}/pull/${s.rec.number}` },
      repository: { type: 'Repository!', resolve: (s) => wrapRepo(s.repo) },
      comments: {
        type: 'IssueCommentConnection!', args: { ...CONN_ARGS },
        resolve: (s, args) => gqlPaginate(s.rec.comments.map((c: { id: number; body: string }) => wrapComment(s.repo, s.rec, c)), args),
      },
      files: {
        type: 'PullRequestChangedFileConnection', args: { ...CONN_ARGS },
        resolve: (s, args) => gqlPaginate(pullFiles(s.repo, s.rec).map((f) => ({
          __gqlType: 'PullRequestChangedFile', path: f.filename, additions: f.additions, deletions: f.deletions,
          changeType: f.status === 'added' ? 'ADDED' : f.status === 'removed' ? 'DELETED' : f.status === 'renamed' ? 'RENAMED' : 'MODIFIED',
        })), args),
      },
    },
  });
  S('PullRequestConnection', { kind: 'OBJECT', fields: connFields('PullRequest') });
  S('PullRequestEdge', { kind: 'OBJECT', fields: edgeFields('PullRequest') });
  S('PullRequestChangedFile', {
    kind: 'OBJECT',
    fields: {
      path: { type: 'String!', resolve: (s) => s.path },
      additions: { type: 'Int!', resolve: (s) => s.additions },
      deletions: { type: 'Int!', resolve: (s) => s.deletions },
      changeType: { type: 'PatchStatus!', resolve: (s) => s.changeType },
    },
  });
  S('PullRequestChangedFileConnection', { kind: 'OBJECT', fields: connFields('PullRequestChangedFile') });
  S('PullRequestChangedFileEdge', { kind: 'OBJECT', fields: edgeFields('PullRequestChangedFile') });
  S('IssueOrPullRequest', { kind: 'UNION', possibleTypes: ['Issue', 'PullRequest'], resolveType: (s) => s.__gqlType });

  S('Ref', {
    kind: 'OBJECT',
    fields: {
      id: { type: 'ID!', resolve: (s) => b64e(`Ref:${s.repo.id}:${s.name}`) },
      name: { type: 'String!', resolve: (s) => s.name.replace(/^refs\/(heads|tags)\//, '') },
      prefix: { type: 'String!', resolve: (s) => (s.name.startsWith('refs/tags/') ? 'refs/tags/' : 'refs/heads/') },
      qualifiedName: { type: 'String!', resolve: (s) => s.name },
      target: { type: 'GitObject', resolve: (s) => wrapGitObject(s.repo, s.repo.refs.get(s.name) ?? ZERO_SHA) },
      repository: { type: 'Repository!', resolve: (s) => wrapRepo(s.repo) },
    },
  });
  S('RefConnection', { kind: 'OBJECT', fields: connFields('Ref') });
  S('RefEdge', { kind: 'OBJECT', fields: edgeFields('Ref') });
  S('Commit', {
    kind: 'OBJECT',
    fields: {
      id: { type: 'ID!', resolve: (s) => b64e(`Commit:${s.sha}`) },
      oid: { type: 'GitObjectID!', resolve: (s) => s.sha },
      abbreviatedOid: { type: 'String!', resolve: (s) => s.sha.slice(0, 7) },
      message: { type: 'String!', resolve: (s) => { const o = s.repo.objects.get(s.sha); return o ? parseCommit(o.data).message.replace(/\n$/, '') : ''; } },
      messageHeadline: { type: 'String!', resolve: (s) => { const o = s.repo.objects.get(s.sha); return o ? parseCommit(o.data).message.split('\n')[0] : ''; } },
      tree: { type: 'Tree!', resolve: (s) => { const o = s.repo.objects.get(s.sha); return { __gqlType: 'Tree', repo: s.repo, sha: o ? parseCommit(o.data).tree : ZERO_SHA }; } },
      treeOid: { type: 'GitObjectID!', resolve: (s) => { const o = s.repo.objects.get(s.sha); return o ? parseCommit(o.data).tree : ZERO_SHA; } },
      committedDate: { type: 'GitTimestamp!', resolve: (s) => { const o = s.repo.objects.get(s.sha); return o ? identParts(parseCommit(o.data).committerLine).date : isoOf(EPOCH); } },
      authoredDate: { type: 'GitTimestamp!', resolve: (s) => { const o = s.repo.objects.get(s.sha); return o ? identParts(parseCommit(o.data).authorLine).date : isoOf(EPOCH); } },
      author: { type: 'GitActor', resolve: (s) => { const o = s.repo.objects.get(s.sha); if (!o) return { __gqlType: 'GitActor', name: '', email: '', date: isoOf(EPOCH) }; const p = identParts(parseCommit(o.data).authorLine); return { __gqlType: 'GitActor', name: p.name, email: p.email, date: p.date }; } },
      committer: { type: 'GitActor', resolve: (s) => { const o = s.repo.objects.get(s.sha); if (!o) return { __gqlType: 'GitActor', name: '', email: '', date: isoOf(EPOCH) }; const p = identParts(parseCommit(o.data).committerLine); return { __gqlType: 'GitActor', name: p.name, email: p.email, date: p.date }; } },
      parents: { type: 'CommitConnection!', args: { ...CONN_ARGS }, resolve: (s, args) => { const o = s.repo.objects.get(s.sha); const ps = o ? parseCommit(o.data).parents : []; return gqlPaginate(ps.map((p: string) => ({ __gqlType: 'Commit', repo: s.repo, sha: p })), args); } },
      url: { type: 'URI!', resolve: (s) => `${repoHtmlUrl(s.repo)}/commit/${s.sha}` },
      repository: { type: 'Repository!', resolve: (s) => wrapRepo(s.repo) },
    },
  });
  S('CommitConnection', { kind: 'OBJECT', fields: connFields('Commit') });
  S('CommitEdge', { kind: 'OBJECT', fields: edgeFields('Commit') });
  S('GitActor', { kind: 'OBJECT', fields: { name: { type: 'String', resolve: (s) => s.name }, email: { type: 'String', resolve: (s) => s.email }, date: { type: 'GitTimestamp', resolve: (s) => s.date } } });
  S('Tree', {
    kind: 'OBJECT',
    fields: {
      id: { type: 'ID!', resolve: (s) => b64e(`Tree:${s.sha}`) },
      oid: { type: 'GitObjectID!', resolve: (s) => s.sha },
      entries: {
        type: '[TreeEntry!]!', resolve: (s) => {
          const o = s.repo.objects.get(s.sha);
          return o ? parseTree(o.data).map((e) => ({ __gqlType: 'TreeEntry', repo: s.repo, name: e.name, sha: e.sha, mode: e.mode, type: e.mode === '40000' ? 'tree' : 'blob' })) : [];
        },
      },
      repository: { type: 'Repository!', resolve: (s) => wrapRepo(s.repo) },
    },
  });
  S('TreeEntry', {
    kind: 'OBJECT',
    fields: {
      name: { type: 'String!', resolve: (s) => s.name },
      path: { type: 'String', resolve: (s) => s.name },
      mode: { type: 'Int!', resolve: (s) => parseInt(s.mode, 8) },
      type: { type: 'String!', resolve: (s) => s.type },
      object: { type: 'GitObject', resolve: (s) => wrapGitObject(s.repo, s.sha) },
    },
  });
  S('Blob', {
    kind: 'OBJECT',
    fields: {
      id: { type: 'ID!', resolve: (s) => b64e(`Blob:${s.sha}`) },
      oid: { type: 'GitObjectID!', resolve: (s) => s.sha },
      text: { type: 'String', resolve: (s) => { const o = s.repo.objects.get(s.sha); return o ? o.data.toString('utf8') : null; } },
      byteSize: { type: 'Int!', resolve: (s) => { const o = s.repo.objects.get(s.sha); return o ? o.data.length : 0; } },
      isTruncated: { type: 'Boolean!', resolve: () => false },
      isBinary: { type: 'Boolean', resolve: () => false },
    },
  });
  S('GitObject', { kind: 'INTERFACE', possibleTypes: ['Commit', 'Tree', 'Blob'], resolveType: (s) => s.__gqlType, fields: { id: { type: 'ID!', resolve: () => '' }, oid: { type: 'GitObjectID!', resolve: (s) => s.sha }, abbreviatedOid: { type: 'String!', resolve: (s) => s.sha.slice(0, 7) } } });

  S('Repository', {
    kind: 'OBJECT',
    fields: {
      id: { type: 'ID!', resolve: (s) => repoNodeId(s.repo) },
      databaseId: { type: 'Int', resolve: (s) => s.repo.id },
      name: { type: 'String!', resolve: (s) => s.repo.name },
      nameWithOwner: { type: 'String!', resolve: (s) => `${VIEWER.login}/${s.repo.name}` },
      owner: { type: 'RepositoryOwner!', resolve: () => wrapUser() },
      description: { type: 'String', resolve: (s) => s.repo.description },
      url: { type: 'URI!', resolve: (s) => repoHtmlUrl(s.repo) },
      homepageUrl: { type: 'URI', resolve: () => null },
      isPrivate: { type: 'Boolean!', resolve: (s) => s.repo.isPrivate },
      isFork: { type: 'Boolean!', resolve: () => false },
      isArchived: { type: 'Boolean!', resolve: () => false },
      isTemplate: { type: 'Boolean!', resolve: () => false },
      isEmpty: { type: 'Boolean!', resolve: (s) => s.repo.refs.size === 0 },
      createdAt: { type: 'DateTime!', resolve: (s) => isoOf(EPOCH + s.repo.createdSeq) },
      updatedAt: { type: 'DateTime!', resolve: (s) => isoOf(EPOCH + s.repo.createdSeq) },
      pushedAt: { type: 'DateTime', resolve: (s) => isoOf(EPOCH + s.repo.createdSeq) },
      defaultBranchRef: {
        type: 'Ref',
        resolve: (s) => (s.repo.refs.has(`refs/heads/${s.repo.defaultBranch}`) ? { __gqlType: 'Ref', repo: s.repo, name: `refs/heads/${s.repo.defaultBranch}` } : null),
      },
      viewerPermission: { type: 'RepositoryPermission!', resolve: () => 'ADMIN' },
      hasIssuesEnabled: { type: 'Boolean!', resolve: () => true },
      hasProjectsEnabled: { type: 'Boolean!', resolve: () => false },
      hasWikiEnabled: { type: 'Boolean!', resolve: () => false },
      stargazerCount: { type: 'Int!', resolve: () => 0 },
      forkCount: { type: 'Int!', resolve: () => 0 },
      diskUsage: { type: 'Int', resolve: () => 0 },
      primaryLanguage: { type: 'Language', resolve: () => null },
      licenseInfo: { type: 'License', resolve: () => null },
      issues: {
        type: 'IssueConnection!', args: { ...CONN_ARGS, states: '[IssueState!]', orderBy: 'IssueOrder', labels: '[String!]', filterBy: 'IssueFilters' },
        resolve: (s, args) => {
          let items = s.repo.issues.filter((r: IssueRec) => r.kind === 'issue');
          if (Array.isArray(args.states) && args.states.length) { const want = args.states.map((x: string) => String(x).toLowerCase()); items = items.filter((r: IssueRec) => want.includes(r.state)); }
          const fb = args.filterBy as { states?: string[] } | undefined;
          if (fb && Array.isArray(fb.states) && fb.states.length) { const want = fb.states.map((x) => String(x).toLowerCase()); items = items.filter((r: IssueRec) => want.includes(r.state)); }
          if (Array.isArray(args.labels) && args.labels.length) items = items.filter((r: IssueRec) => args.labels.every((l: string) => r.labels.includes(l)));
          return gqlPaginate(sortByOrder(items, args).map((r) => wrapIssue(s.repo, r)), args);
        },
      },
      issue: { type: 'Issue', args: { number: 'Int!' }, resolve: (s, args) => { const rec = s.repo.issues.find((r: IssueRec) => r.kind === 'issue' && r.number === args.number); return rec ? wrapIssue(s.repo, rec) : null; } },
      issueOrPullRequest: {
        type: 'IssueOrPullRequest', args: { number: 'Int!' },
        resolve: (s, args) => {
          const rec = s.repo.issues.find((r: IssueRec) => r.number === args.number);
          if (!rec) return null;
          return rec.kind === 'pull' ? wrapPr(s.repo, rec) : wrapIssue(s.repo, rec);
        },
      },
      pullRequests: {
        type: 'PullRequestConnection!', args: { ...CONN_ARGS, states: '[PullRequestState!]', orderBy: 'IssueOrder', baseRefName: 'String', headRefName: 'String', labels: '[String!]' },
        resolve: (s, args) => {
          let items = s.repo.issues.filter((r: IssueRec) => r.kind === 'pull');
          if (Array.isArray(args.states) && args.states.length) items = items.filter((r: IssueRec) => args.states.some((x: string) => (x === 'MERGED' ? !!r.merged : !r.merged && r.state === String(x).toLowerCase())));
          if (typeof args.baseRefName === 'string') items = items.filter((r: IssueRec) => r.baseRef === args.baseRefName);
          if (typeof args.headRefName === 'string') items = items.filter((r: IssueRec) => r.headRef === args.headRefName);
          return gqlPaginate(sortByOrder(items, args).map((r) => wrapPr(s.repo, r)), args);
        },
      },
      pullRequest: { type: 'PullRequest', args: { number: 'Int!' }, resolve: (s, args) => { const rec = s.repo.issues.find((r: IssueRec) => r.kind === 'pull' && r.number === args.number); return rec ? wrapPr(s.repo, rec) : null; } },
      refs: {
        type: 'RefConnection', args: { ...CONN_ARGS, refPrefix: 'String!' },
        resolve: (s, args) => {
          const prefix = String(args.refPrefix ?? 'refs/heads/');
          const names = [...s.repo.refs.keys()].filter((k) => k.startsWith(prefix)).sort();
          return gqlPaginate(names.map((k) => ({ __gqlType: 'Ref', repo: s.repo, name: k })), args);
        },
      },
      ref: {
        type: 'Ref', args: { qualifiedName: 'String!' },
        resolve: (s, args) => {
          const q = String(args.qualifiedName);
          const full = q.startsWith('refs/') ? q : `refs/heads/${q}`;
          return s.repo.refs.has(full) ? { __gqlType: 'Ref', repo: s.repo, name: full } : null;
        },
      },
      object: {
        type: 'GitObject', args: { expression: 'String!' },
        resolve: (s, args) => {
          const expr = String(args.expression);
          const colon = expr.indexOf(':');
          const rev = colon >= 0 ? expr.slice(0, colon) : expr;
          const path = colon >= 0 ? expr.slice(colon + 1) : '';
          let sha: string | undefined;
          if (/^[0-9a-f]{40}$/.test(rev)) sha = rev;
          else if (rev === 'HEAD') sha = branchTip(s.repo, s.repo.defaultBranch);
          else sha = branchTip(s.repo, rev) ?? s.repo.refs.get(rev.startsWith('refs/') ? rev : `refs/heads/${rev}`);
          if (!sha) return null;
          const obj = s.repo.objects.get(sha);
          if (obj && obj.type === 'commit') { if (!path) return wrapGitObject(s.repo, sha); sha = parseCommit(obj.data).tree; }
          if (!path) return wrapGitObject(s.repo, sha);
          let cur = sha;
          for (const part of path.split('/').filter(Boolean)) {
            const o = s.repo.objects.get(cur);
            if (!o || o.type !== 'tree') return null;
            const entry = parseTree(o.data).find((e) => e.name === part);
            if (!entry) return null;
            cur = entry.sha;
          }
          return wrapGitObject(s.repo, cur);
        },
      },
      watchers: { type: 'UserConnection!', args: { ...CONN_ARGS }, resolve: (_s, args) => gqlPaginate([], args) },
      stargazers: { type: 'StargazerConnection!', args: { ...CONN_ARGS }, resolve: (_s, args) => gqlPaginate([], args) },
    },
  });
  S('UserConnection', { kind: 'OBJECT', fields: connFields('User') });
  S('UserEdge', { kind: 'OBJECT', fields: edgeFields('User') });
  S('StargazerConnection', { kind: 'OBJECT', fields: connFields('User') });
  S('StargazerEdge', { kind: 'OBJECT', fields: edgeFields('User') });

  S('SearchResultItemConnection', {
    kind: 'OBJECT',
    fields: {
      ...connFields('SearchResultItem'),
      issueCount: { type: 'Int!', resolve: (s) => s.counts.issue },
      repositoryCount: { type: 'Int!', resolve: (s) => s.counts.repository },
      userCount: { type: 'Int!', resolve: () => 0 },
      wikiCount: { type: 'Int!', resolve: () => 0 },
      codeCount: { type: 'Int!', resolve: () => 0 },
      discussionCount: { type: 'Int!', resolve: () => 0 },
    },
  });
  S('SearchResultItemEdge', { kind: 'OBJECT', fields: edgeFields('SearchResultItem') });
  S('SearchResultItem', { kind: 'UNION', possibleTypes: ['Issue', 'PullRequest', 'Repository', 'User'], resolveType: (s) => s.__gqlType });

  S('RateLimit', {
    kind: 'OBJECT',
    fields: {
      limit: { type: 'Int!', resolve: () => 5000 },
      cost: { type: 'Int!', resolve: () => 1 },
      remaining: { type: 'Int!', resolve: () => 4999 },
      used: { type: 'Int!', resolve: () => 1 },
      resetAt: { type: 'DateTime!', resolve: () => isoOf(EPOCH + 3600) },
      nodeCount: { type: 'Int!', resolve: () => 0 },
    },
  });

  S('Query', {
    kind: 'OBJECT',
    fields: {
      viewer: { type: 'User!', resolve: () => wrapUser() },
      repository: {
        type: 'Repository', args: { owner: 'String!', name: 'String!' },
        resolve: (_s, args) => {
          const repo = findRepo(String(args.owner), String(args.name));
          return repo ? wrapRepo(repo) : null;
        },
      },
      node: { type: 'Node', args: { id: 'ID!' }, resolve: (_s, args) => decodeNodeId(String(args.id)) },
      nodes: { type: '[Node]!', args: { ids: '[ID!]!' }, resolve: (_s, args) => (Array.isArray(args.ids) ? args.ids.map((id: string) => decodeNodeId(String(id))) : []) },
      rateLimit: { type: 'RateLimit', args: { dryRun: 'Boolean' }, resolve: () => ({ __gqlType: 'RateLimit' }) },
      search: {
        type: 'SearchResultItemConnection!', args: { query: 'String!', type: 'SearchType!', ...CONN_ARGS },
        resolve: (_s, args) => {
          const q = String(args.query ?? '');
          const type = String(args.type ?? 'ISSUE');
          let repoFilter: Repo | undefined;
          const terms: string[] = [];
          for (const tok of q.split(/\s+/).filter(Boolean)) {
            if (tok.startsWith('repo:')) { const [o, n] = tok.slice(5).split('/'); if (o && n) repoFilter = findRepo(o, n); }
            else if (tok.startsWith('type:') || tok.startsWith('in:') || tok.startsWith('is:') || tok.startsWith('state:')) continue;
            else terms.push(tok.toLowerCase());
          }
          const matches = (text: string): boolean => terms.every((t) => text.toLowerCase().includes(t));
          const items: any[] = [];
          const pool = repoFilter ? [repoFilter] : [...repos.values()];
          if (type === 'REPOSITORY') for (const r of pool) if (matches(r.name)) items.push(wrapRepo(r));
          if (type === 'ISSUE') for (const r of pool) for (const rec of r.issues.filter((x) => x.kind === 'issue')) if (matches(rec.title)) items.push(wrapIssue(r, rec));
          if (type === 'PULL_REQUEST') for (const r of pool) for (const rec of r.issues.filter((x) => x.kind === 'pull')) if (matches(rec.title)) items.push(wrapPr(r, rec));
          if (type === 'USER') items.push(wrapUser());
          const page = gqlPaginate(items, args);
          return { ...page, counts: { issue: type === 'ISSUE' ? items.length : 0, repository: type === 'REPOSITORY' ? items.length : 0 } };
        },
      },
    },
  });

  const input = (name: string, fields: Record<string, string>): void => { S(name, { kind: 'INPUT_OBJECT', inputFields: fields }); };
  input('CreateIssueInput', { repositoryId: 'ID!', title: 'String!', body: 'String', clientMutationId: 'String' });
  input('CloseIssueInput', { issueId: 'ID!', stateReason: 'IssueClosedStateReason', clientMutationId: 'String' });
  input('CreatePullRequestInput', { repositoryId: 'ID!', baseRefName: 'String!', headRefName: 'String!', title: 'String!', body: 'String', draft: 'Boolean', clientMutationId: 'String' });
  input('MergePullRequestInput', { pullRequestId: 'ID!', mergeMethod: 'PullRequestMergeMethod', commitHeadline: 'String', clientMutationId: 'String' });
  input('AddCommentInput', { subjectId: 'ID!', body: 'String!', clientMutationId: 'String' });
  input('IssueOrder', { field: 'IssueOrderField!', direction: 'OrderDirection!' });
  input('IssueFilters', { states: '[IssueState!]', labels: '[String!]', since: 'DateTime' });
  input('RepositoryOrder', { field: 'RepositoryOrderField!', direction: 'OrderDirection!' });
  S('CreateIssuePayload', { kind: 'OBJECT', fields: { issue: { type: 'Issue', resolve: (s) => s.issue }, clientMutationId: { type: 'String', resolve: (s) => s.clientMutationId ?? null } } });
  S('CloseIssuePayload', { kind: 'OBJECT', fields: { issue: { type: 'Issue', resolve: (s) => s.issue }, clientMutationId: { type: 'String', resolve: (s) => s.clientMutationId ?? null } } });
  S('CreatePullRequestPayload', { kind: 'OBJECT', fields: { pullRequest: { type: 'PullRequest', resolve: (s) => s.pullRequest }, clientMutationId: { type: 'String', resolve: (s) => s.clientMutationId ?? null } } });
  S('MergePullRequestPayload', { kind: 'OBJECT', fields: { pullRequest: { type: 'PullRequest', resolve: (s) => s.pullRequest }, clientMutationId: { type: 'String', resolve: (s) => s.clientMutationId ?? null } } });
  S('AddCommentPayload', { kind: 'OBJECT', fields: { commentEdge: { type: 'IssueCommentEdge', resolve: (s) => s.commentEdge }, subject: { type: 'Node', resolve: (s) => s.subject }, clientMutationId: { type: 'String', resolve: (s) => s.clientMutationId ?? null } } });

  S('Mutation', {
    kind: 'OBJECT',
    fields: {
      createIssue: {
        type: 'CreateIssuePayload', args: { input: 'CreateIssueInput!' },
        resolve: (_s, args) => {
          const inp = args.input ?? {};
          const target = decodeNodeId(String(inp.repositoryId ?? ''));
          if (!target || target.__gqlType !== 'Repository') throw new Error('repository not found');
          const repo: Repo = target.repo;
          const rec: IssueRec = { kind: 'issue', number: repo.nextNumber++, id: nextIssueId++, title: String(inp.title ?? ''), body: typeof inp.body === 'string' ? inp.body : '', state: 'open', labels: [], comments: [], createdSeq: ++seq, ver: 0 };
          repo.issues.push(rec);
          return { __gqlType: 'CreateIssuePayload', issue: wrapIssue(repo, rec), clientMutationId: inp.clientMutationId };
        },
      },
      closeIssue: {
        type: 'CloseIssuePayload', args: { input: 'CloseIssueInput!' },
        resolve: (_s, args) => {
          const inp = args.input ?? {};
          const target = decodeNodeId(String(inp.issueId ?? ''));
          if (!target || target.__gqlType !== 'Issue') throw new Error('issue not found');
          target.rec.state = 'closed';
          target.rec.ver += 1; // V5b: state feeds issueJson — invalidate the cached REST body
          return { __gqlType: 'CloseIssuePayload', issue: target, clientMutationId: inp.clientMutationId };
        },
      },
      createPullRequest: {
        type: 'CreatePullRequestPayload', args: { input: 'CreatePullRequestInput!' },
        resolve: (_s, args) => {
          const inp = args.input ?? {};
          const target = decodeNodeId(String(inp.repositoryId ?? ''));
          if (!target || target.__gqlType !== 'Repository') throw new Error('repository not found');
          const repo: Repo = target.repo;
          const headRef = String(inp.headRefName ?? ''); const baseRef = String(inp.baseRefName ?? '');
          if (!branchTip(repo, headRef)) throw new Error(`head ref ${headRef} does not exist`);
          if (!branchTip(repo, baseRef)) throw new Error(`base ref ${baseRef} does not exist`);
          const rec: IssueRec = { kind: 'pull', number: repo.nextNumber++, id: nextIssueId++, title: String(inp.title ?? ''), body: typeof inp.body === 'string' ? inp.body : '', state: 'open', labels: [], comments: [], createdSeq: ++seq, ver: 0, headRef, baseRef, merged: false, mergeCommitSha: null };
          repo.issues.push(rec);
          return { __gqlType: 'CreatePullRequestPayload', pullRequest: wrapPr(repo, rec), clientMutationId: inp.clientMutationId };
        },
      },
      mergePullRequest: {
        type: 'MergePullRequestPayload', args: { input: 'MergePullRequestInput!' },
        resolve: (_s, args) => {
          const inp = args.input ?? {};
          const target = decodeNodeId(String(inp.pullRequestId ?? ''));
          if (!target || target.__gqlType !== 'PullRequest') throw new Error('pull request not found');
          const sha = mergePullRec(target.repo, target.rec);
          if (sha === null) throw new Error('pull request is not mergeable');
          return { __gqlType: 'MergePullRequestPayload', pullRequest: target, clientMutationId: inp.clientMutationId };
        },
      },
      addComment: {
        type: 'AddCommentPayload', args: { input: 'AddCommentInput!' },
        resolve: (_s, args) => {
          const inp = args.input ?? {};
          const target = decodeNodeId(String(inp.subjectId ?? ''));
          if (!target || (target.__gqlType !== 'Issue' && target.__gqlType !== 'PullRequest')) throw new Error('subject not found');
          const comment = { id: nextIssueId++, body: String(inp.body ?? '') };
          target.rec.comments.push(comment);
          target.rec.ver += 1; // V5b: comments.length feeds issueJson — invalidate the cached REST body
          const wrapped = wrapComment(target.repo, target.rec, comment);
          return { __gqlType: 'AddCommentPayload', commentEdge: { node: wrapped, cursor: b64e(`cursor:${target.rec.comments.length - 1}`) }, subject: target, clientMutationId: inp.clientMutationId };
        },
      },
    },
  });

  S('IssueState', { kind: 'ENUM', enumValues: ['OPEN', 'CLOSED'] });
  S('IssueStateReason', { kind: 'ENUM', enumValues: ['COMPLETED', 'NOT_PLANNED', 'REOPENED'] });
  S('IssueClosedStateReason', { kind: 'ENUM', enumValues: ['COMPLETED', 'NOT_PLANNED'] });
  S('PullRequestState', { kind: 'ENUM', enumValues: ['OPEN', 'CLOSED', 'MERGED'] });
  S('PullRequestMergeMethod', { kind: 'ENUM', enumValues: ['MERGE', 'SQUASH', 'REBASE'] });
  S('MergeableState', { kind: 'ENUM', enumValues: ['CONFLICTING', 'MERGEABLE', 'UNKNOWN'] });
  S('OrderDirection', { kind: 'ENUM', enumValues: ['ASC', 'DESC'] });
  S('IssueOrderField', { kind: 'ENUM', enumValues: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'] });
  S('RepositoryOrderField', { kind: 'ENUM', enumValues: ['CREATED_AT', 'UPDATED_AT', 'PUSHED_AT', 'NAME', 'STARGAZERS'] });
  S('SearchType', { kind: 'ENUM', enumValues: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'] });
  S('RepositoryPermission', { kind: 'ENUM', enumValues: ['ADMIN', 'MAINTAIN', 'READ', 'TRIAGE', 'WRITE'] });
  S('PatchStatus', { kind: 'ENUM', enumValues: ['ADDED', 'CHANGED', 'COPIED', 'DELETED', 'MODIFIED', 'RENAMED'] });
  S('Language', { kind: 'OBJECT', fields: { id: { type: 'ID!', resolve: () => '' }, name: { type: 'String!', resolve: () => '' }, color: { type: 'String', resolve: () => null } } });
  S('License', { kind: 'OBJECT', fields: { id: { type: 'ID!', resolve: () => '' }, name: { type: 'String!', resolve: () => '' }, spdxId: { type: 'String', resolve: () => null }, key: { type: 'String!', resolve: () => '' } } });

  buildIntrospection(schema);
  return schema;
}
const FORGE_SCHEMA = buildForgeSchema();

// ---------------------------------------------------------------- GraphQL endpoint + MCP (V3)
function handleGraphQL(res: ServerResponse, raw: Buffer): void {
  let parsed: unknown;
  try { parsed = JSON.parse(raw.toString('utf8')); } catch { sendJson(res, 400, { errors: [{ message: 'Problems parsing JSON' }] }); return; }
  const runOne = (item: unknown): unknown => {
    if (typeof item !== 'object' || item === null || Array.isArray(item)) return { errors: [{ message: 'invalid request' }] };
    const body = item as { query?: unknown; variables?: unknown; operationName?: unknown };
    if (typeof body.query !== 'string') return { errors: [{ message: 'query is required' }] };
    const vars = (typeof body.variables === 'object' && body.variables !== null ? body.variables : {}) as Record<string, unknown>;
    return gqlRun(FORGE_SCHEMA, body.query, vars, typeof body.operationName === 'string' ? body.operationName : undefined);
  };
  if (Array.isArray(parsed)) { sendJson(res, 200, parsed.map(runOne)); return; }
  sendJson(res, 200, runOne(parsed));
}

const MCP_PROTOCOL_VERSIONS = ['2026-07-28', '2025-06-18', '2025-03-26', '2024-11-05'];
function mcpToolsList(): Record<string, unknown>[] {
  const obj = (properties: Record<string, unknown>, required: string[]): Record<string, unknown> => ({ type: 'object', properties, required });
  const str = { type: 'string' };
  return [
    { name: 'create_repo', description: 'Create a new repository owned by the authenticated user.', inputSchema: obj({ name: str, description: str, private: { type: 'boolean' } }, ['name']) },
    { name: 'get_repo', description: 'Get a repository by owner and name.', inputSchema: obj({ owner: str, repo: str }, ['owner', 'repo']) },
    { name: 'create_issue', description: 'Create an issue in a repository.', inputSchema: obj({ owner: str, repo: str, title: str, body: str }, ['owner', 'repo', 'title']) },
    { name: 'list_issues', description: 'List issues in a repository.', inputSchema: obj({ owner: str, repo: str, state: str }, ['owner', 'repo']) },
    { name: 'create_pull_request', description: 'Open a pull request from a head branch into a base branch.', inputSchema: obj({ owner: str, repo: str, title: str, head: str, base: str, body: str }, ['owner', 'repo', 'title', 'head', 'base']) },
    { name: 'merge_pull_request', description: 'Merge an open pull request.', inputSchema: obj({ owner: str, repo: str, pull_number: { type: 'number' } }, ['owner', 'repo', 'pull_number']) },
  ];
}
function mcpToolResult(data: unknown): Record<string, unknown> {
  return { content: [{ type: 'text', text: JSON.stringify(data) }], structuredContent: (typeof data === 'object' && data !== null && !Array.isArray(data) ? data : { result: data }) as Record<string, unknown>, isError: false };
}
function mcpToolError(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: message }], isError: true };
}
function mcpCallTool(name: string, args: Record<string, unknown>): { result?: Record<string, unknown>; error?: { code: number; message: string } } {
  const tools = mcpToolsList();
  const tool = tools.find((t) => t.name === name);
  if (!tool) return { error: { code: -32602, message: `unknown tool: ${name}` } };
  const schema = tool.inputSchema as { required?: string[] };
  for (const req of schema.required ?? []) {
    if (args[req] === undefined || args[req] === null || args[req] === '') return { error: { code: -32602, message: `missing required argument: ${req}` } };
  }
  const repoOf = (): Repo | undefined => findRepo(String(args.owner ?? ''), String(args.repo ?? ''));
  switch (name) {
    case 'create_repo': {
      const repo = newRepo(String(args.name), Boolean(args.private), typeof args.description === 'string' ? args.description : '');
      return { result: mcpToolResult(repoJson(repo)) };
    }
    case 'get_repo': {
      const repo = repoOf();
      if (!repo) return { result: mcpToolError('repository not found') };
      return { result: mcpToolResult(repoJson(repo)) };
    }
    case 'create_issue': {
      const repo = repoOf();
      if (!repo) return { result: mcpToolError('repository not found') };
      const rec: IssueRec = { kind: 'issue', number: repo.nextNumber++, id: nextIssueId++, title: String(args.title), body: typeof args.body === 'string' ? args.body : '', state: 'open', labels: [], comments: [], createdSeq: ++seq, ver: 0 };
      repo.issues.push(rec);
      return { result: mcpToolResult(issueJson(repo, rec)) };
    }
    case 'list_issues': {
      const repo = repoOf();
      if (!repo) return { result: mcpToolError('repository not found') };
      const state = typeof args.state === 'string' ? args.state : 'open';
      let list = repo.issues.filter((i) => i.kind === 'issue');
      if (state === 'open') list = list.filter((i) => i.state === 'open');
      else if (state === 'closed') list = list.filter((i) => i.state === 'closed');
      return { result: mcpToolResult(list.map((i) => issueJson(repo, i))) };
    }
    case 'create_pull_request': {
      const repo = repoOf();
      if (!repo) return { result: mcpToolError('repository not found') };
      const head = String(args.head).replace(new RegExp(`^${VIEWER.login}:`), '');
      const base = String(args.base);
      if (!branchTip(repo, head)) return { result: mcpToolError('head branch does not exist') };
      if (!branchTip(repo, base)) return { result: mcpToolError('base branch does not exist') };
      const rec: IssueRec = { kind: 'pull', number: repo.nextNumber++, id: nextIssueId++, title: String(args.title), body: typeof args.body === 'string' ? args.body : '', state: 'open', labels: [], comments: [], createdSeq: ++seq, ver: 0, headRef: head, baseRef: base, merged: false, mergeCommitSha: null };
      repo.issues.push(rec);
      return { result: mcpToolResult(pullJson(repo, rec)) };
    }
    case 'merge_pull_request': {
      const repo = repoOf();
      if (!repo) return { result: mcpToolError('repository not found') };
      const rec = repo.issues.find((i) => i.kind === 'pull' && i.number === Number(args.pull_number));
      if (!rec) return { result: mcpToolError('pull request not found') };
      const sha = mergePullRec(repo, rec);
      if (sha === null) return { result: mcpToolError('pull request is not mergeable') };
      return { result: mcpToolResult({ ...pullJson(repo, rec), sha, merged: true }) };
    }
  }
  return { error: { code: -32602, message: `unknown tool: ${name}` } };
}
function mcpHandleOne(msg: Record<string, unknown>): Record<string, unknown> | null {
  const id = msg.id;
  const method = typeof msg.method === 'string' ? msg.method : '';
  const isNotification = id === undefined || id === null;
  const ok = (result: unknown): Record<string, unknown> => ({ jsonrpc: '2.0', id: id ?? null, result });
  const fail = (code: number, message: string): Record<string, unknown> => ({ jsonrpc: '2.0', id: id ?? null, error: { code, message } });
  switch (method) {
    case 'server/discover':
      return ok({ _meta: {}, protocolVersion: '2026-07-28', supportedVersions: MCP_PROTOCOL_VERSIONS, capabilities: { tools: {} }, serverInfo: { name: 'forge-mcp', version: '1.0.0' } });
    case 'initialize': {
      const params = (msg.params ?? {}) as { protocolVersion?: unknown };
      const wanted = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
      const version = MCP_PROTOCOL_VERSIONS.includes(wanted) ? wanted : '2025-06-18';
      return ok({ protocolVersion: version, capabilities: { tools: { listChanged: false } }, serverInfo: { name: 'forge-mcp', version: '1.0.0' } });
    }
    case 'ping': return ok({});
    case 'tools/list': return ok({ tools: mcpToolsList() });
    case 'tools/call': {
      const params = (msg.params ?? {}) as { name?: unknown; arguments?: unknown };
      if (typeof params.name !== 'string') return fail(-32602, 'missing tool name');
      const toolArgs = (typeof params.arguments === 'object' && params.arguments !== null ? params.arguments : {}) as Record<string, unknown>;
      const outcome = mcpCallTool(params.name, toolArgs);
      if (outcome.error) return fail(outcome.error.code, outcome.error.message);
      return ok(outcome.result);
    }
    default:
      if (isNotification) return null;
      return fail(-32601, `method not found: ${method}`);
  }
}
function handleMcp(res: ServerResponse, raw: Buffer): void {
  let parsed: unknown;
  try { parsed = JSON.parse(raw.toString('utf8')); } catch {
    res.writeHead(400, { 'content-type': JSON_TYPE });
    res.end(JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } }));
    return;
  }
  if (Array.isArray(parsed)) {
    const out = parsed.map((m) => (typeof m === 'object' && m !== null ? mcpHandleOne(m as Record<string, unknown>) : null)).filter((x) => x !== null);
    sendJson(res, 200, out);
    return;
  }
  if (typeof parsed !== 'object' || parsed === null) {
    res.writeHead(400, { 'content-type': JSON_TYPE });
    res.end(JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } }));
    return;
  }
  const out = mcpHandleOne(parsed as Record<string, unknown>);
  if (out === null) { res.writeHead(202, { 'content-type': JSON_TYPE }); res.end(''); return; }
  sendJson(res, 200, out);
}
