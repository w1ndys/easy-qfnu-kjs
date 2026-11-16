// 公开页页脚（tasklist 第 6 项；Requirement 5.6、PRD 6.2 末条）：
// 一行说明数据来源、更新频率与「仅供参考」。公开查询页与全天状态页共用。
//
// 不写死具体的同步钟点：同步计划由管理面板配置（PRD 5.5），页脚只承诺「每周自动同步」，
// 免得页面上出现一个后端一改就过期的时刻。

import { Typography } from "antd";

/** 页脚一行数据说明；内容来自 PRD 6.2 的固定口径。 */
const FOOTER_TEXT =
  "数据来自学校教务系统，每周自动同步；数据仅供参考，请以教务系统为准。";

export function PageFooter() {
  return (
    <footer
      style={{
        marginTop: 24,
        paddingTop: 12,
        borderTop: "1px solid rgba(0, 0, 0, 0.06)",
      }}
    >
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {FOOTER_TEXT}
      </Typography.Text>
    </footer>
  );
}
