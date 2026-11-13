package collector

import "context"

// execute 是一轮采集的执行体：从读设置开始，到发布或失败收口为止。
//
// 本项（tasklist 第 5 项）只交付调用壳：阶段顺序（读设置 → 登录 → 学期/周历 →
// 替换日历 → 绑学期 → 写窗口 → 快照 → hydrate → 两段执行 → 发布）由第 6–10 项填充。
// 在那之前返回 nil，表示执行体没有做任何收口动作——轮次保持 running，与管理端看到的
// 「这一轮还在跑」一致，也符合「调用方取消/进程退出时保持 running」的语义（CP8）。
//
// 占位实现刻意不写终态：本项不承担任何阶段，凭空写一条 failed 会让面板出现一条
// 解释不了原因的失败轮次。真正的失败收口必须落在第 6–10 项各自的失败点上。
//
// 参数用下划线是因为占位实现不消费它们：unparam 会把未使用的参数报成 lint 失败，
// 第 6 项填充时再改回 ctx / runID。
func (c *Collector) execute(_ context.Context, _ int64) error {
	return nil
}
