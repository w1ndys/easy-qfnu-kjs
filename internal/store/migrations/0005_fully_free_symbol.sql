-- “完全空闲”不是格子符号：真实矩阵里的空白格统一由清洗层判成 free，
-- 教务的“完全空闲”只在筛选条件里按查询范围对整间教室成立，
-- 查询侧用区间内每一节 available 聚合表达，不写进逐格观测。
-- 0003 曾把它登记成 dict_symbol；这里删掉，避免把未预期文本误判成可用状态。
-- 依据 docs/decisions/2026-10-23-fully-free-is-range-aggregate.md。

DELETE FROM dict_symbol
 WHERE dict_version = 1
   AND symbol = '完全空闲';
