// 由 scripts/generate-data-index.mjs 自动生成 —— 请勿手工编辑。
// 数据源：data/manifest.json + data/terms/<term>/weeks/week-*.json。
// 每项映射一份已发布周快照：sha256 供运行时校验文件与索引一致；path 相对仓库根。

export const term = "2025-2026-3";
export const releaseId = "20260904T002919+0800";
export const weeks: Record<string, { week: number; snapshotId: string; sha256: string; path: string }> = {
  "1": { week: 1, snapshotId: "20260904T002919+0800-week-01", sha256: "8ba77f8fd8467e3ef158c58d8365a0a4dc3f86cf1cf2ed6e262923e3a2342309", path: "data/terms/2025-2026-3/weeks/week-01.json" },
  "2": { week: 2, snapshotId: "20260904T002919+0800-week-02", sha256: "2eec6c73436986d4ca19f1a276b8632a0ec14b9eac9050ea5259768cb25051f7", path: "data/terms/2025-2026-3/weeks/week-02.json" },
  "3": { week: 3, snapshotId: "20260904T002919+0800-week-03", sha256: "f21dba1b3624a10d7d22278a440e9f43aed6637aa2426a01d125bf8f291f37c2", path: "data/terms/2025-2026-3/weeks/week-03.json" },
  "4": { week: 4, snapshotId: "20260904T002919+0800-week-04", sha256: "9832d137a711940fa2a04341fbaf867c39b6676cbe11fb5c6e20338fa1286cd4", path: "data/terms/2025-2026-3/weeks/week-04.json" },
};
