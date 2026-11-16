// 时间展示单点收敛（design D8）：入参一律 RFC3339 UTC，出参一律 Asia/Shanghai。
// 只有这一处写 timeZone；页面里再出现第二种格式化路径就违反 Correctness Properties 7。
// 不引 dayjs 插件：原生 Intl 的 formatToParts 够用，也不用关心宿主机时区。

/** 展示时区：后台时间戳按 UTC 传输，界面按上海时区展示（PRD 7.1）。 */
const DISPLAY_TIME_ZONE = "Asia/Shanghai";

/**
 * 只用 formatToParts 取字段再自己拼：不依赖 locale 的分隔符与字段顺序。
 * 小时用 h23，避免午夜被格式化成某些 locale 的 24:00。
 */
const formatter = new Intl.DateTimeFormat("en-US", {
  timeZone: DISPLAY_TIME_ZONE,
  year: "numeric",
  month: "2-digit",
  day: "2-digit",
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
});

/** 取一个格式化片段；缺失时返回空串，让结果至少还是可读的字符串而不是 undefined。 */
function part(parts: Intl.DateTimeFormatPart[], type: string): string {
  return parts.find((item) => item.type === type)?.value ?? "";
}

/**
 * RFC3339 UTC 时间 → `YYYY-MM-DD HH:mm`（Asia/Shanghai）。
 *
 * 空值与无法解析的输入返回 null：契约里的时间保证可解析，真拿到坏值时由调用方
 * 决定显示「未知」，这里不猜一个替代值，也不把原始 ISO 串漏到界面上。
 */
export function formatShanghaiDateTime(
  value: string | null | undefined,
): string | null {
  if (value === null || value === undefined || value === "") {
    return null;
  }

  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return null;
  }

  const parts = formatter.formatToParts(date);
  const datePart = [
    part(parts, "year"),
    part(parts, "month"),
    part(parts, "day"),
  ].join("-");
  const timePart = [part(parts, "hour"), part(parts, "minute")].join(":");
  return `${datePart} ${timePart}`;
}
