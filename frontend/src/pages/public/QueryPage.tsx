import { Card, Typography } from "antd";

// 第 3–6 项会在这里落筛选区、结果列表与四态；本项只保证路由可达、antd 可用。
export function QueryPage() {
  return (
    <main style={{ padding: 24 }}>
      <Typography.Title level={2}>空教室查询</Typography.Title>
      <Card>
        <Typography.Paragraph>
          页面骨架占位：日期、教学楼、节次、关键词筛选与结果区在第 3–6 项实现。
        </Typography.Paragraph>
      </Card>
    </main>
  );
}
