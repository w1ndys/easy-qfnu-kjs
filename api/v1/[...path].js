// api-lib/availability.ts
var AVAILABLE_STATUS_IDS = /* @__PURE__ */ new Set([5, 8]);
function roomFreeInRange(statuses, startNode, endNode) {
  for (let i = startNode; i <= endNode; i++) {
    const code = String(i).padStart(2, "0");
    const status = statuses[code];
    if (status === void 0 || !AVAILABLE_STATUS_IDS.has(status)) return false;
  }
  return true;
}

// api-lib/data.ts
import { createHash } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

// api-lib/errors.ts
var CODES = {
  invalidParameter: "invalid_parameter",
  notCollected: "not_collected",
  notInTeaching: "not_in_teaching",
  noSnapshot: "no_snapshot",
  notFound: "not_found",
  methodNotAllowed: "method_not_allowed",
  internal: "internal"
};
var TITLES = {
  invalid_parameter: "Invalid parameter",
  not_collected: "Not collected",
  not_in_teaching: "Not in teaching period",
  no_snapshot: "No snapshot available",
  not_found: "Not Found",
  method_not_allowed: "Method Not Allowed",
  internal: "Internal Server Error"
};
var ApiError = class extends Error {
  constructor(status, code, detail, extraHeaders) {
    super(detail);
    this.status = status;
    this.code = code;
    this.extraHeaders = extraHeaders;
    this.name = "ApiError";
  }
  status;
  code;
  extraHeaders;
};
function problemResponse(status, code, detail, instance, extraHeaders) {
  const body = {
    type: "about:blank",
    title: TITLES[code],
    status,
    detail,
    instance,
    code
  };
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      "Content-Type": "application/problem+json; charset=utf-8",
      ...extraHeaders ?? {}
    }
  });
}
function errorToResponse(err, instance) {
  if (err instanceof ApiError) {
    return problemResponse(err.status, err.code, err.message, instance, err.extraHeaders);
  }
  return problemResponse(500, CODES.internal, "\u670D\u52A1\u5185\u90E8\u9519\u8BEF", instance);
}

// api-lib/generated-index.ts
var term = "";
var releaseId = "";
var weeks = {};

// api-lib/data.ts
var caches = /* @__PURE__ */ new Map();
var HERE = path.dirname(fileURLToPath(import.meta.url));
function resolveRoot() {
  const envRoot = process.env.EASY_KJS_ROOT;
  if (envRoot) return path.resolve(envRoot);
  let dir = HERE;
  for (; ; ) {
    if (existsSync(path.join(dir, "data"))) return dir;
    const parent = path.dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  return path.resolve(process.cwd());
}
function indexSourceKey() {
  const override = process.env.EASY_KJS_INDEX_JSON;
  return override ? path.resolve(override) : "static";
}
function readIndexFromEnv(file) {
  let raw;
  try {
    raw = JSON.parse(readFileSync(file, "utf8"));
  } catch {
    throw new ApiError(503, CODES.internal, "\u6570\u636E\u7D22\u5F15\u6587\u4EF6\u89E3\u6790\u5931\u8D25");
  }
  const idx = raw;
  if (typeof idx !== "object" || idx === null || typeof idx.term !== "string" || typeof idx.releaseId !== "string" || typeof idx.weeks !== "object" || idx.weeks === null) {
    throw new ApiError(503, CODES.internal, "\u6570\u636E\u7D22\u5F15\u6587\u4EF6\u683C\u5F0F\u65E0\u6548");
  }
  return idx;
}
function cache() {
  const root = resolveRoot();
  const indexKey = indexSourceKey();
  const key = `${root}\0${indexKey}`;
  let c = caches.get(key);
  if (!c) {
    c = {
      root,
      indexKey,
      index: indexKey === "static" ? { term, releaseId, weeks } : readIndexFromEnv(indexKey),
      metaLoaded: false,
      manifest: null,
      weeks: /* @__PURE__ */ new Map()
    };
    caches.set(key, c);
  }
  return c;
}
function internal(detail) {
  return new ApiError(503, CODES.internal, detail);
}
function getManifest() {
  const c = cache();
  if (c.metaLoaded) return c.manifest;
  c.metaLoaded = true;
  let raw;
  try {
    raw = readFileSync(path.join(c.root, "data", "manifest.json"), "utf8");
  } catch (err) {
    const code = err.code;
    if (code === "ENOENT") {
      c.manifest = null;
      return null;
    }
    throw internal("manifest.json \u8BFB\u53D6\u5931\u8D25");
  }
  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw internal("manifest.json \u89E3\u6790\u5931\u8D25");
  }
  if (parsed.schema_version !== 1) {
    throw internal("manifest schema_version \u4E0D\u53D7\u5F53\u524D API \u652F\u6301");
  }
  if (c.index.term && c.index.term !== parsed.term) {
    throw internal("\u6784\u5EFA\u671F\u6570\u636E\u7D22\u5F15\u4E0E manifest \u5B66\u671F\u4E0D\u4E00\u81F4");
  }
  c.manifest = parsed;
  return parsed;
}
function getWeekFile(week) {
  const c = cache();
  if (!c.metaLoaded) {
    getManifest();
  }
  const key = String(week);
  if (c.weeks.has(key)) return c.weeks.get(key) ?? null;
  const entry = c.index.weeks[key];
  if (!entry) {
    c.weeks.set(key, null);
    return null;
  }
  const filePath = path.join(c.root, entry.path);
  let bytes;
  try {
    bytes = readFileSync(filePath);
  } catch (err) {
    const code = err.code;
    if (code === "ENOENT") {
      c.weeks.set(key, null);
      return null;
    }
    throw internal(`\u7B2C ${week} \u5468\u5FEB\u7167\u8BFB\u53D6\u5931\u8D25`);
  }
  const digest = createHash("sha256").update(bytes).digest("hex");
  if (digest !== entry.sha256) {
    throw internal(`\u7B2C ${week} \u5468\u5FEB\u7167 SHA-256 \u4E0E\u7D22\u5F15\u4E0D\u4E00\u81F4`);
  }
  let file;
  try {
    file = JSON.parse(bytes.toString("utf8"));
  } catch {
    throw internal(`\u7B2C ${week} \u5468\u5FEB\u7167 JSON \u89E3\u6790\u5931\u8D25`);
  }
  if (file.schema_version !== 1 || file.week !== week || file.snapshot_id !== entry.snapshotId) {
    throw internal(`\u7B2C ${week} \u5468\u5FEB\u7167\u5143\u6570\u636E\u4E0E\u7D22\u5F15\u4E0D\u4E00\u81F4`);
  }
  const manifest = c.manifest;
  if (manifest && file.term !== manifest.term) {
    throw internal(`\u7B2C ${week} \u5468\u5FEB\u7167\u5B66\u671F\u4E0E manifest \u4E0D\u4E00\u81F4`);
  }
  c.weeks.set(key, file);
  return file;
}

// api-lib/keyword.ts
function asciiLower(s) {
  return s.replace(/[A-Z]/g, (c) => c.toLowerCase());
}
function normalizeKeyword(input) {
  return asciiLower(input.normalize("NFKC").trim());
}
function keywordLength(keyword) {
  return Array.from(keyword).length;
}
function keywordMatchesName(name, normalizedKeyword) {
  return asciiLower(name).includes(normalizedKeyword);
}

// api-lib/params.ts
var NODE_RE = /^(0[1-9]|1[0-2])$/;
var WEEK_RE = /^\d{1,2}$/;
var DAY_RE = /^[1-7]$/;
function requireGroup(searchParams, manifest) {
  const groupId = searchParams.get("group_id") ?? "";
  if (groupId === "") {
    throw new ApiError(400, CODES.invalidParameter, "\u7F3A\u5C11\u5FC5\u586B\u53C2\u6570 group_id");
  }
  if (!manifest.groups.some((g) => g.id === groupId)) {
    const available = manifest.groups.map((g) => g.id).join("\u3001");
    throw new ApiError(
      404,
      CODES.notCollected,
      `\u5206\u7EC4 ${groupId} \u672A\u6536\u5F55${available ? `\uFF0C\u53EF\u7528\u5206\u7EC4\uFF1A${available}` : ""}`
    );
  }
  return groupId;
}
function parseKeyword(searchParams) {
  const raw = searchParams.get("keyword");
  if (raw === null) return null;
  const normalized = normalizeKeyword(raw);
  if (normalized === "") return null;
  if (keywordLength(normalized) > 32) {
    throw new ApiError(400, CODES.invalidParameter, "keyword \u6700\u591A 32 \u4E2A\u5B57\u7B26");
  }
  return normalized;
}
function parseWeek(value, totalWeeks) {
  if (!WEEK_RE.test(value)) {
    throw new ApiError(400, CODES.invalidParameter, "week \u5FC5\u987B\u4E3A 1\u201330 \u7684\u6574\u6570");
  }
  const n = Number(value);
  if (n < 1 || n > 30) {
    throw new ApiError(400, CODES.invalidParameter, "week \u5FC5\u987B\u5728 1\u201330 \u4E4B\u95F4");
  }
  if (totalWeeks > 0 && n > totalWeeks) {
    throw new ApiError(
      400,
      CODES.invalidParameter,
      `week \u8D85\u51FA\u672C\u5B66\u671F\u8303\u56F4\uFF081\u2013${totalWeeks}\uFF09`
    );
  }
  return n;
}
function parseDay(value) {
  if (!DAY_RE.test(value)) {
    throw new ApiError(400, CODES.invalidParameter, "day \u5FC5\u987B\u4E3A 1\u20137 \u7684\u6574\u6570");
  }
  return Number(value);
}
function parseQueryBase(searchParams, manifest, ctx) {
  const groupId = requireGroup(searchParams, manifest);
  const keyword = parseKeyword(searchParams);
  const rawWeek = searchParams.get("week");
  let week;
  if (rawWeek === null) {
    if (ctx.week === null) {
      throw new ApiError(
        400,
        CODES.notInTeaching,
        "\u5F53\u524D\u4E0D\u5728\u6559\u5B66\u5468\u5185\uFF0C\u7701\u7565 week \u65E0\u6CD5\u786E\u5B9A\u67E5\u8BE2\u5468\u6B21\uFF0C\u8BF7\u663E\u5F0F\u6307\u5B9A week"
      );
    }
    week = ctx.week;
  } else {
    week = parseWeek(rawWeek, manifest.anchor.total_weeks);
  }
  const rawDay = searchParams.get("day");
  const day = rawDay === null ? ctx.weekday : parseDay(rawDay);
  return { groupId, keyword, week, day };
}
function parseNodeSpan(searchParams) {
  const startRaw = searchParams.get("start_node");
  const endRaw = searchParams.get("end_node");
  const startNode = startRaw === null ? "01" : startRaw;
  const endNode = endRaw === null ? "12" : endRaw;
  if (!NODE_RE.test(startNode)) {
    throw new ApiError(400, CODES.invalidParameter, "start_node \u5FC5\u987B\u4E3A 01\u201312 \u7684\u4E24\u4F4D\u6570\u5B57");
  }
  if (!NODE_RE.test(endNode)) {
    throw new ApiError(400, CODES.invalidParameter, "end_node \u5FC5\u987B\u4E3A 01\u201312 \u7684\u4E24\u4F4D\u6570\u5B57");
  }
  if (Number(startNode) > Number(endNode)) {
    throw new ApiError(400, CODES.invalidParameter, "start_node \u4E0D\u80FD\u665A\u4E8E end_node");
  }
  return { startNode, endNode };
}

// api-lib/time.ts
var TERM_TIMEZONE = "Asia/Shanghai";
var DATE_PARTS_FMT = new Intl.DateTimeFormat("en-US", {
  timeZone: TERM_TIMEZONE,
  year: "numeric",
  month: "2-digit",
  day: "2-digit"
});
function requestNow() {
  const frozen = process.env.EASY_KJS_NOW;
  if (frozen) {
    const t = Date.parse(frozen);
    if (!Number.isNaN(t)) return t;
  }
  return Date.now();
}
function shanghaiCivilDate(now) {
  const parts = DATE_PARTS_FMT.formatToParts(now);
  let year = 0;
  let month = 0;
  let day = 0;
  for (const p of parts) {
    if (p.type === "year") year = Number(p.value);
    else if (p.type === "month") month = Number(p.value);
    else if (p.type === "day") day = Number(p.value);
  }
  return { year, month, day };
}
function weekdayOf(d) {
  const jsDay = new Date(Date.UTC(d.year, d.month - 1, d.day)).getUTCDay();
  return jsDay === 0 ? 7 : jsDay;
}
function formatCivilDate(d) {
  const mm = String(d.month).padStart(2, "0");
  const dd = String(d.day).padStart(2, "0");
  return `${d.year}-${mm}-${dd}`;
}
function parseCivilDate(s) {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s);
  if (!m) return null;
  const year = Number(m[1]);
  const month = Number(m[2]);
  const day = Number(m[3]);
  if (month < 1 || month > 12 || day < 1 || day > 31) return null;
  const probe = new Date(Date.UTC(year, month - 1, day));
  if (probe.getUTCFullYear() !== year || probe.getUTCMonth() !== month - 1 || probe.getUTCDate() !== day) {
    return null;
  }
  return { year, month, day };
}
function dayNumber(d) {
  return Math.floor(Date.UTC(d.year, d.month - 1, d.day) / 864e5);
}
function addDays(d, n) {
  const dt = new Date(Date.UTC(d.year, d.month - 1, d.day + n));
  return {
    year: dt.getUTCFullYear(),
    month: dt.getUTCMonth() + 1,
    day: dt.getUTCDate()
  };
}
function mondaysStrictlyAfter(a, b) {
  if (dayNumber(b) <= dayNumber(a)) return 0;
  let count = 0;
  let cur = addDays(a, 1);
  while (dayNumber(cur) <= dayNumber(b)) {
    if (weekdayOf(cur) === 1) count += 1;
    cur = addDays(cur, 1);
  }
  return count;
}
function computeContext(term2, anchor, now) {
  const today = shanghaiCivilDate(now);
  const date = formatCivilDate(today);
  const weekday = weekdayOf(today);
  const notTeaching = {
    date,
    weekday,
    week: null,
    term: term2,
    in_teaching_calendar: false
  };
  const anchorDate = parseCivilDate(anchor.date);
  if (!anchorDate) return notTeaching;
  if (!anchor.in_teaching_calendar || anchor.total_weeks < 1 || anchor.week < 1) {
    return notTeaching;
  }
  const crossed = mondaysStrictlyAfter(anchorDate, today);
  const week = anchor.week + crossed;
  if (week < 1 || week > anchor.total_weeks) return notTeaching;
  return { date, weekday, week, term: term2, in_teaching_calendar: true };
}

// api-lib/sort.ts
function tokenize(s) {
  return s.match(/\d+|\D+/g) ?? [];
}
function compareText(a, b) {
  const la = asciiLowerLocal(a);
  const lb = asciiLowerLocal(b);
  const n = Math.min(la.length, lb.length);
  for (let i = 0; i < n; i++) {
    const ca = la.charCodeAt(i);
    const cb = lb.charCodeAt(i);
    if (ca !== cb) return ca < cb ? -1 : 1;
  }
  if (la.length !== lb.length) return la.length < lb.length ? -1 : 1;
  if (a !== b) return a < b ? -1 : 1;
  return 0;
}
function asciiLowerLocal(s) {
  return s.replace(/[A-Z]/g, (c) => c.toLowerCase());
}
function naturalCompare(a, b) {
  const ta = tokenize(a);
  const tb = tokenize(b);
  const n = Math.min(ta.length, tb.length);
  for (let i = 0; i < n; i++) {
    const ca = ta[i];
    const cb = tb[i];
    const aNum = /^\d+$/.test(ca);
    const bNum = /^\d+$/.test(cb);
    if (aNum && bNum) {
      const diff2 = Number(ca) - Number(cb);
      if (diff2 !== 0) return diff2 < 0 ? -1 : 1;
      continue;
    }
    if (aNum !== bNum) return aNum ? -1 : 1;
    const diff = compareText(ca, cb);
    if (diff !== 0) return diff;
  }
  if (ta.length !== tb.length) return ta.length < tb.length ? -1 : 1;
  return compareText(a, b);
}

// api-lib/staleness.ts
var RECENT_WINDOW_WEEKS = 4;
var RECENT_THRESHOLD_MS = 36 * 60 * 60 * 1e3;
var FAR_THRESHOLD_MS = 8 * 24 * 60 * 60 * 1e3;
function weekStatus(entry, opts) {
  if (!entry) return "missing";
  const success = Date.parse(entry.last_success_at);
  if (Number.isNaN(success)) return "stale";
  if (entry.last_error_code != null) {
    if (entry.last_attempt_at == null) return "stale";
    const attempt = Date.parse(entry.last_attempt_at);
    if (Number.isNaN(attempt) || attempt > success) return "stale";
  }
  const recent = opts.currentWeek != null && entry.week >= opts.currentWeek && entry.week <= opts.currentWeek + RECENT_WINDOW_WEEKS;
  const age = opts.now - success;
  return age > (recent ? RECENT_THRESHOLD_MS : FAR_THRESHOLD_MS) ? "stale" : "fresh";
}

// api-lib/views.ts
function buildManifestView(manifest, currentWeek, now) {
  const weeks2 = [];
  for (let w = 1; w <= manifest.anchor.total_weeks; w++) {
    const entry = manifest.weeks[String(w)];
    weeks2.push({
      week: w,
      generated_at: entry?.generated_at ?? null,
      last_success_at: entry?.last_success_at ?? null,
      status: weekStatus(entry, { currentWeek, now })
    });
  }
  return {
    schema_version: manifest.schema_version,
    term: manifest.term,
    release_id: manifest.release_id,
    generated_at: manifest.generated_at,
    anchor: manifest.anchor,
    groups: manifest.groups,
    nodes: manifest.nodes,
    statuses: manifest.statuses,
    weeks: weeks2
  };
}
function buildSnapshotView(entry, weekFile, currentWeek, now) {
  const status = weekStatus(entry, { currentWeek, now });
  return {
    snapshot_id: weekFile.snapshot_id,
    generated_at: weekFile.generated_at,
    stale: status === "stale"
  };
}
function byNameThenId(a, b) {
  const byName = naturalCompare(a.name, b.name);
  if (byName !== 0) return byName;
  return naturalCompare(a.id, b.id);
}
function buildRoomList(rooms) {
  return rooms.map((r) => ({ id: r.jsbh, name: r.name })).sort(byNameThenId);
}
function buildFullDayList(rooms) {
  return rooms.map((r) => ({ id: r.jsbh, name: r.name, statuses: { ...r.statuses } })).sort(byNameThenId);
}

// function-src/v1/[...path].ts
var CACHE_DATA = "public, s-maxage=300, stale-while-revalidate=600";
var CACHE_CONTEXT = "public, s-maxage=60";
var KNOWN_RESOURCES = {
  manifest: true,
  context: true,
  "empty-classrooms": true,
  "full-day-status": true
};
function json(data, cacheControl) {
  return new Response(JSON.stringify(data), {
    status: 200,
    headers: {
      "Content-Type": "application/json; charset=utf-8",
      "Cache-Control": cacheControl
    }
  });
}
function handleManifest() {
  console.error("[probe] manifest-handler");
  const manifest = getManifest();
  if (!manifest) {
    return problemResponse(503, CODES.noSnapshot, "\u5C1A\u672A\u53D1\u5E03\u4EFB\u4F55\u5FEB\u7167\u6570\u636E", "/api/v1/manifest");
  }
  console.error("[probe] ctx-before");
  const now = requestNow();
  const ctx = computeContext(manifest.term, manifest.anchor, new Date(now));
  console.error("[probe] ctx-after", JSON.stringify(ctx));
  const body = JSON.stringify(buildManifestView(manifest, ctx.week, now));
  console.error("[probe] view-after", body.length);
  return json(body, CACHE_DATA);
}
function handleContext() {
  const manifest = getManifest();
  if (!manifest) {
    return problemResponse(503, CODES.noSnapshot, "\u5C1A\u672A\u53D1\u5E03\u4EFB\u4F55\u5FEB\u7167\u6570\u636E", "/api/v1/context");
  }
  const ctx = computeContext(manifest.term, manifest.anchor, new Date(requestNow()));
  return json(
    {
      date: ctx.date,
      weekday: ctx.weekday,
      week: ctx.week,
      term: ctx.term,
      in_teaching_calendar: ctx.in_teaching_calendar
    },
    CACHE_CONTEXT
  );
}
function loadDayRooms(manifest, base) {
  const manifestEntry = manifest.weeks[String(base.week)];
  if (!manifestEntry) {
    throw new ApiError(404, CODES.notCollected, `\u7B2C ${base.week} \u5468\u5C1A\u672A\u6536\u5F55`);
  }
  const weekFile = getWeekFile(base.week);
  if (!weekFile) {
    throw new ApiError(404, CODES.notCollected, `\u7B2C ${base.week} \u5468\u5FEB\u7167\u6587\u4EF6\u7F3A\u5931`);
  }
  const dayRooms = weekFile.days[String(base.day)];
  if (!dayRooms) {
    throw new ApiError(
      404,
      CODES.notCollected,
      `\u7B2C ${base.week} \u5468\u661F\u671F ${base.day} \u672A\u6536\u5F55`
    );
  }
  return {
    manifestEntry,
    weekFile,
    rooms: dayRooms.rooms.filter((r) => r.group_id === base.groupId)
  };
}
function handleEmptyClassrooms(manifest, ctx, searchParams) {
  const now = requestNow();
  const base = parseQueryBase(searchParams, manifest, ctx);
  const { startNode, endNode } = parseNodeSpan(searchParams);
  const { manifestEntry, weekFile, rooms } = loadDayRooms(manifest, base);
  const matched = rooms.filter((r) => base.keyword === null || keywordMatchesName(r.name, base.keyword)).filter((r) => roomFreeInRange(r.statuses, Number(startNode), Number(endNode)));
  const roomList = buildRoomList(matched);
  return json(
    {
      query: {
        group_id: base.groupId,
        keyword: base.keyword,
        week: base.week,
        day: base.day,
        start_node: startNode,
        end_node: endNode
      },
      rooms: roomList,
      count: roomList.length,
      snapshot: buildSnapshotView(manifestEntry, weekFile, ctx.week, now)
    },
    CACHE_DATA
  );
}
function handleFullDayStatus(manifest, ctx, searchParams) {
  const now = requestNow();
  const base = parseQueryBase(searchParams, manifest, ctx);
  const { manifestEntry, weekFile, rooms } = loadDayRooms(manifest, base);
  const matched = rooms.filter(
    (r) => base.keyword === null || keywordMatchesName(r.name, base.keyword)
  );
  const roomList = buildFullDayList(matched);
  return json(
    {
      query: {
        group_id: base.groupId,
        keyword: base.keyword,
        week: base.week,
        day: base.day
      },
      rooms: roomList,
      snapshot: buildSnapshotView(manifestEntry, weekFile, ctx.week, now)
    },
    CACHE_DATA
  );
}
function handle(req) {
  console.error("[probe] handle-enter", req.method, req.url);
  const url = new URL(req.url, "https://vercel.app");
  const segments = url.pathname.split("/").filter(Boolean);
  const instance = url.pathname + url.search;
  if (segments.length !== 3 || segments[0] !== "api" || segments[1] !== "v1") {
    return problemResponse(404, CODES.notFound, "\u672A\u77E5\u7684 API \u8DEF\u5F84", instance);
  }
  const resource = segments[2];
  if (!KNOWN_RESOURCES[resource]) {
    return problemResponse(404, CODES.notFound, `\u672A\u77E5\u8D44\u6E90 ${resource}`, instance);
  }
  if (req.method !== "GET") {
    return problemResponse(405, CODES.methodNotAllowed, "\u8BE5\u8D44\u6E90\u4EC5\u652F\u6301 GET", instance, {
      Allow: "GET"
    });
  }
  try {
    console.error("[probe] before-manifest");
    const manifest = getManifest();
    console.error("[probe] manifest", manifest ? manifest.term : null);
    const ctx = manifest ? computeContext(manifest.term, manifest.anchor, new Date(requestNow())) : null;
    switch (resource) {
      case "manifest":
        return handleManifest();
      case "context":
        return handleContext();
      case "empty-classrooms": {
        if (!manifest || !ctx) {
          return problemResponse(
            503,
            CODES.noSnapshot,
            "\u5C1A\u672A\u53D1\u5E03\u4EFB\u4F55\u5FEB\u7167\u6570\u636E\uFF0C\u65E0\u6CD5\u67E5\u8BE2",
            instance
          );
        }
        return handleEmptyClassrooms(manifest, ctx, url.searchParams);
      }
      case "full-day-status": {
        if (!manifest || !ctx) {
          return problemResponse(
            503,
            CODES.noSnapshot,
            "\u5C1A\u672A\u53D1\u5E03\u4EFB\u4F55\u5FEB\u7167\u6570\u636E\uFF0C\u65E0\u6CD5\u67E5\u8BE2",
            instance
          );
        }
        return handleFullDayStatus(manifest, ctx, url.searchParams);
      }
      default:
        return problemResponse(404, CODES.notFound, `\u672A\u77E5\u8D44\u6E90 ${resource}`, instance);
    }
  } catch (err) {
    return errorToResponse(err, instance);
  }
}
async function handler(req) {
  return handle(req);
}
export {
  handler as default
};
