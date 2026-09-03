// 自然排序（决策文件 §11.3）：房间按名称自然排序，名称相同时按 jsbh 排序。
// 自然排序把连续数字段按数值比较（10 在 2 之后）；非数字段按 ASCII 小写后的码点比较，
// 结果跨环境确定（不依赖 ICU 排序规则）。

function tokenize(s: string): string[] {
  return s.match(/\d+|\D+/g) ?? [];
}

function compareText(a: string, b: string): number {
  const la = asciiLowerLocal(a);
  const lb = asciiLowerLocal(b);
  const n = Math.min(la.length, lb.length);
  for (let i = 0; i < n; i++) {
    const ca = la.charCodeAt(i);
    const cb = lb.charCodeAt(i);
    if (ca !== cb) return ca < cb ? -1 : 1;
  }
  if (la.length !== lb.length) return la.length < lb.length ? -1 : 1;
  // 同一字母不同大小写等情形：按原串码点给出确定次序。
  if (a !== b) return a < b ? -1 : 1;
  return 0;
}

function asciiLowerLocal(s: string): string {
  return s.replace(/[A-Z]/g, (c) => c.toLowerCase());
}

/** 自然比较两字符串；返回负数/0/正数。 */
export function naturalCompare(a: string, b: string): number {
  const ta = tokenize(a);
  const tb = tokenize(b);
  const n = Math.min(ta.length, tb.length);
  for (let i = 0; i < n; i++) {
    const ca = ta[i]!;
    const cb = tb[i]!;
    const aNum = /^\d+$/.test(ca);
    const bNum = /^\d+$/.test(cb);
    if (aNum && bNum) {
      const diff = Number(ca) - Number(cb);
      if (diff !== 0) return diff < 0 ? -1 : 1;
      continue;
    }
    if (aNum !== bNum) return aNum ? -1 : 1;
    const diff = compareText(ca, cb);
    if (diff !== 0) return diff;
  }
  if (ta.length !== tb.length) return ta.length < tb.length ? -1 : 1;
  return compareText(a, b);
}
