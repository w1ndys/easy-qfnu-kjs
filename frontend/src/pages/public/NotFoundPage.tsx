import { Result } from "antd";
import { Link } from "react-router";

// 404 与根错误页共用 antd Result 结构（design.md「路由」）。
export function NotFoundPage() {
  return (
    <Result
      status="404"
      title="页面不存在"
      subTitle="链接可能已经失效，回到空教室查询继续。"
      extra={<Link to="/">返回空教室查询</Link>}
    />
  );
}
