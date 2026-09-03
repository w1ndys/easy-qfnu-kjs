// 由 scripts/generate-data-index.mjs 自动生成 —— 请勿手工编辑。
// 数据源：data/manifest.json + data/terms/<term>/weeks/week-*.json。
// 每项映射一份已发布周快照：sha256 供运行时校验文件与索引一致；path 相对仓库根。

export const term = "";
export const releaseId = "";
export const weeks: Record<string, { week: number; snapshotId: string; sha256: string; path: string }> = {};
