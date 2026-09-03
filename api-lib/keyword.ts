// keyword 规范化与匹配（决策文件 §11.1）：
//   去首尾空格 → Unicode NFKC → ASCII 字母大小写不敏感 → 普通子串匹配（不支持正则）。

export function asciiLower(s: string): string {
  return s.replace(/[A-Z]/g, (c) => c.toLowerCase());
}

/** 归一化用户输入的关键词；空串表示“无过滤”。 */
export function normalizeKeyword(input: string): string {
  return asciiLower(input.normalize('NFKC').trim());
}

/** keyword 长度按 Unicode 码点计（≤32 字符）。 */
export function keywordLength(keyword: string): number {
  return Array.from(keyword).length;
}

/** 普通子串匹配：房间名按 ASCII 小写后包含归一化关键词。 */
export function keywordMatchesName(name: string, normalizedKeyword: string): boolean {
  return asciiLower(name).includes(normalizedKeyword);
}
