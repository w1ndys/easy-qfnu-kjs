-- 种子数据：dict 版本 1 与 axis 版本 1 是契约里写死的初始分类与节次轴，
-- 来源 docs/contract/data-format.v2.md 状态字典种子值与 docs/upstream.md 第 4 节。
-- 符号必须原样写入，码点不得规范化成 ASCII（Ｊ/Ｘ/Ｇ/Ｌ 是全角拉丁字母，Κ 是希腊大写 Kappa）。

INSERT INTO dict (version, note)
VALUES (1, '种子：docs/contract/data-format.v2.md 状态表')
ON CONFLICT (version) DO NOTHING;

INSERT INTO dict_state (dict_version, state_key, label, available) VALUES
  (1, 'class',            '正常上课',   false),
  (1, 'borrowed',         '借用',       false),
  (1, 'locked',           '锁定',       false),
  (1, 'exam',             '考试',       false),
  (1, 'free',             '空闲',       true),
  (1, 'fixed_reschedule', '固定调课',   false),
  (1, 'temp_reschedule',  '临时调课',   false),
  (1, 'fully_free',       '完全空闲',   true),
  (1, 'cross_mode',       '跨模式占用', false),
  (1, 'composite',        '复合占用',   false),
  (1, 'unknown',          '未知',       false)
ON CONFLICT (dict_version, state_key) DO NOTHING;

-- 符号 → 状态键。空单元格归 free 由清洗层判断，不在这里登记符号。
INSERT INTO dict_symbol (dict_version, symbol, state_key) VALUES
  (1, '◆',       'class'),
  (1, 'Ｊ',      'borrowed'),
  (1, 'Ｘ',      'locked'),
  (1, 'Κ',       'exam'),
  (1, '空闲',    'free'),
  (1, 'Ｇ',      'fixed_reschedule'),
  (1, 'Ｌ',      'temp_reschedule'),
  (1, '完全空闲', 'fully_free'),
  (1, 'M',       'cross_mode')
ON CONFLICT (dict_version, symbol) DO NOTHING;

INSERT INTO axis (version, note)
VALUES (1, '种子：5 大节 → 12 小节（docs/upstream.md 第 4 节）')
ON CONFLICT (version) DO NOTHING;

INSERT INTO axis_node (axis_version, node, ordinal, block, block_ordinal) VALUES
  (1, '01',  1, '0102',   1),
  (1, '02',  2, '0102',   1),
  (1, '03',  3, '030405', 2),
  (1, '04',  4, '030405', 2),
  (1, '05',  5, '030405', 2),
  (1, '06',  6, '0607',   3),
  (1, '07',  7, '0607',   3),
  (1, '08',  8, '0809',   4),
  (1, '09',  9, '0809',   4),
  (1, '10', 10, '101112', 5),
  (1, '11', 11, '101112', 5),
  (1, '12', 12, '101112', 5)
ON CONFLICT (axis_version, node) DO NOTHING;

-- 采集调度的默认表达式：每天 04:10（robfig/cron v3 六段式含秒），管理员可在面板修改
INSERT INTO settings (key, value)
VALUES ('cron_expr', '0 10 4 * * *')
ON CONFLICT (key) DO NOTHING;
