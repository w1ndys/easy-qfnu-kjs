// 空教室可用性判定（决策文件 §11.1 / Q59）：
// 所选 day 快照中 start..end 每个小节的状态 ∈ {5, 8} 才视为可用；
// 缺失小节视为不可用（未知状态绝不映射为空闲）。
export const AVAILABLE_STATUS_IDS = new Set<number>([5, 8]);

export function roomFreeInRange(
  statuses: Record<string, number>,
  startNode: number,
  endNode: number,
): boolean {
  for (let i = startNode; i <= endNode; i++) {
    const code = String(i).padStart(2, '0');
    const status = statuses[code];
    if (status === undefined || !AVAILABLE_STATUS_IDS.has(status)) return false;
  }
  return true;
}

/** 该日房间状态缺少数值的小节（01–12 中不在 statuses 键内的）。 */
export function missingNodes(statuses: Record<string, number>): string[] {
  const missing: string[] = [];
  for (let i = 1; i <= 12; i++) {
    const code = String(i).padStart(2, '0');
    if (statuses[code] === undefined) missing.push(code);
  }
  return missing;
}
