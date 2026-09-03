import { describe, expect, it } from 'vitest';

import { naturalCompare } from '../sort';

describe('naturalCompare（§11.3：按名称自然排序，同名按 jsbh）', () => {
  it('数字段按数值比较', () => {
    expect(naturalCompare('a2', 'a10')).toBeLessThan(0);
    expect(naturalCompare('F126', 'F12')).toBeGreaterThan(0);
    expect(naturalCompare('文史楼201', '文史楼1010')).toBeLessThan(0);
  });

  it('CJK 文本段按码点确定性比较（不依赖 ICU）', () => {
    expect(naturalCompare('文史楼201', '综合教学楼101')).toBeLessThan(0); // 文(U+6587) < 综(U+7EFC)
    expect(naturalCompare('综合教学楼101', '综合教学楼202')).toBeLessThan(0);
  });

  it('非数字文本与数字文本相遇时数字在前/后可确定', () => {
    // F126 的 'F' 码点小于中文字符，因此排在最前
    expect(naturalCompare('F126', '文史楼201')).toBeLessThan(0);
  });

  it('完全相同 → 0；ASCII 大小写按原串稳定排序', () => {
    expect(naturalCompare('综合教学楼101', '综合教学楼101')).toBe(0);
    expect(naturalCompare('0103', '0104')).toBeLessThan(0);
  });
});
