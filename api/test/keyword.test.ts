import { describe, expect, it } from 'vitest';

import {
  keywordLength,
  keywordMatchesName,
  normalizeKeyword,
} from '../lib/keyword';

describe('keyword 规范化与子串匹配（§11.1）', () => {
  it('NFKC + 去首尾空格（含全角空格与全角字母数字）', () => {
    expect(normalizeKeyword('　Ｆ１２６　')).toBe('f126');
    expect(normalizeKeyword('  综合教学楼Ａ101  ')).toBe('综合教学楼a101');
  });

  it('ASCII 字母大小写不敏感', () => {
    expect(normalizeKeyword('F126')).toBe('f126');
    expect(normalizeKeyword('f126')).toBe('f126');
  });

  it('CJK 与纯数字保持原样', () => {
    expect(normalizeKeyword('综合教学楼101')).toBe('综合教学楼101');
  });

  it('普通子串匹配（非正则、ASCII 大小写不敏感）', () => {
    expect(keywordMatchesName('综合教学楼101', normalizeKeyword('综合教学楼'))).toBe(true);
    expect(keywordMatchesName('F126', normalizeKeyword('f126'))).toBe(true);
    expect(keywordMatchesName('F126', normalizeKeyword('Ｆ１２６'))).toBe(true);
    expect(keywordMatchesName('文史楼201', normalizeKeyword('101'))).toBe(false);
    // 正则元字符按字面处理，不会“匹配任意”
    expect(keywordMatchesName('综合教学楼101', normalizeKeyword('.*'))).toBe(false);
  });

  it('keyword 长度按码点计算', () => {
    expect(keywordLength(normalizeKeyword('长'.repeat(33)))).toBe(33);
    expect(keywordLength(normalizeKeyword('ab'))).toBe(2);
  });
});
