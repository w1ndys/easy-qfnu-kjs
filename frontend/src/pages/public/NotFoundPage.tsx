import { Result } from "antd";
import { Link } from "react-router";
import { PageFooter } from "../../components/PageFooter";

// 404 与根错误页共用 antd Result 结构（design.md「路由」）。
// 页脚与另两个公开页一致：Requirement 5.6 的口径是「任一公开页面」。
export function NotFoundPage() {
  return (
    <>
      <main style={{ padding: 24, maxWidth: 960, margin: "0 auto" }}>
        <Result
          status="404"
          title="页面不存在"
          subTitle="链接可能已经失效，回到空教室查询继续。"
          extra={<Link to="/">返回空教室查询</Link>}
        />
      </main>
      {/* 页脚在 <main> 之外：<footer> 只有不嵌在 main/section 里才映射成 contentinfo。 */}
      <div style={{ maxWidth: 960, margin: "0 auto", padding: "0 24px 24px" }}>
        <PageFooter />
      </div>
    </>
  );
}
