import { Card, Typography } from "antd";

// 第 7–9 项会在这里落暗色等比缩放布局、热力图与 30 秒轮询。
export function ScreenPage() {
  return (
    <main style={{ padding: 24 }}>
      <Typography.Title level={2}>数据大屏</Typography.Title>
      <Card>
        <Typography.Paragraph>
          页面骨架占位：站点规模、版本、新鲜度、占用热力图在第 7–9 项实现。
        </Typography.Paragraph>
      </Card>
    </main>
  );
}
