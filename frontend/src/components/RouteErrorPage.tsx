import { Button, Result, Space } from "antd";
import { Link } from "react-router";

// createBrowserRouter 的根 errorElement：任何路由子树渲染/加载抛错都由它接管，
// 所以页面崩溃时看到的是这张错误页而不是白屏。不展示原始异常信息，避免把
// 内部细节带到页面上（Requirement 7.6 的同一条口径）。
export function RouteErrorPage() {
  return (
    <Result
      status="error"
      title="页面出错了"
      subTitle="页面渲染失败，数据没有显示出来。可以重新加载，或回到空教室查询。"
      extra={
        <Space size="middle">
          <Button type="primary" onClick={() => window.location.reload()}>
            重新加载
          </Button>
          <Link to="/">返回空教室查询</Link>
        </Space>
      }
    />
  );
}
