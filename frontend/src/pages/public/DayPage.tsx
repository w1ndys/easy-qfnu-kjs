import { Card, Typography } from "antd";
import { useParams } from "react-router";

// 第 4–6 项会在这里落 12 小节网格与房间头；本项只验证 roomId 路由参数能取到。
export function DayPage() {
  const { roomId } = useParams<{ roomId: string }>();

  return (
    <main style={{ padding: 24 }}>
      <Typography.Title level={2}>全天状态</Typography.Title>
      <Card>
        <Typography.Paragraph>
          页面骨架占位：房间标识 {roomId ?? "未提供"}，12 小节网格在第 4–6
          项实现。
        </Typography.Paragraph>
      </Card>
    </main>
  );
}
