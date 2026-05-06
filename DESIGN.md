# Frontend Design Specification

## Project Understanding

### 业务理解

- 产品核心价值：把曲阜师范大学教务系统里分散、难检索的教室占用信息，转换成学生可直接操作的 Web 查询工具，降低找空教室、判断某栋楼全天占用情况、理解整体使用热度的时间成本。
- 行业/领域：高校教务信息服务，属于校园数字化工具与教育场景效率产品。
- 目标用户：以在校学生为主，也可能覆盖辅导员、社团负责人、备课教师等轻度管理角色；整体技术水平中低到中等，主要通过手机浏览器访问，高频诉求是“快、准、少学习成本”。
- 核心使用场景：
  - 课间或自习前的即时空教室查询，属于高频、低容错、移动优先操作。
  - 查看某教学楼某天全天占用节奏，属于中频的信息比对场景。
  - 运营方查看查询热度、搜索词和趋势分析，属于低频但信息密度较高的后台分析场景。

### 技术理解

- 当前前端技术栈：`Vue 3` + `Vue Router 4` + `Axios` + `Vite` + `Vant 4`（移动端组件库）。
- 数据可视化依赖：`ECharts 6` 直接使用 `echarts/core` 按需引入。
- 设计相关依赖：使用 Vant 4 作为基础组件库，通过 CSS 变量覆盖 Vant 主题实现品牌定制；不再使用 Tailwind CSS。
- 组件按需引入：通过 `unplugin-vue-components` + `@vant/auto-import-resolver` 实现 Vant 组件自动按需导入。
- 项目结构：
  - 6 个主路由页面：`/`、`/empty-classroom`、`/full-day-status`、`/dashboard`、`/admin/login`、`/admin`
  - 一组通用组件：头部（NavBar）、底部、日期选择器、对话框、空状态、加载态、警告条、统计卡片、公告卡片、二维码卡片
  - 多个 composables 管理状态查询、搜索历史、热搜、公告已读、教学楼别名提醒、弹窗提示等轻量逻辑
- 复杂度判断：整体不是复杂后台系统，但已经具备多页面、多状态、表格、图表、弹窗、历史记录、快捷入口等中等复杂度前端特征。

### 界面理解

- 核心页面/模块：
  - 首页：功能入口、公告、简版统计、品牌承接
  - 空教室查询页：输入教学楼、日期、节次范围，返回教室列表
  - 全天状态页：输入教学楼和日期，返回横向状态矩阵表格
  - 数据大屏页：面向运营/维护的查询趋势、热门关键词、结果分布和高峰时段分析
- 信息密度：
  - 首页与查询页偏中低密度，强调快速理解与操作
  - 全天状态页偏中高密度，核心是横向滚动表格
  - 大屏页是高信息密度图表与指标卡组合
- 交互复杂度：
  - 不是复杂工作流，更接近轻量查询工具
  - 但包含多状态处理：系统权限、教学周历提醒、搜索历史、热搜、错误反馈、图表切换、移动横向表格
- 导航结构：
  - 当前是轻量单层路由
  - 首页为 Hub，二级功能页采用返回式顶栏
  - 不适合传统后台侧栏；更适合移动优先的顶部导航 + 页面内分区导航

### 情感理解

- 产品应传达的感觉：可信、清晰、校园友好、效率导向，而不是炫技或强商业化。
- 最适合的设计风格：
  - 面向学生的公共服务产品，适合“温和的专业感 + 清爽的校园科技感”
  - 可以参考 Apple 教育类信息界面的清晰层次、Notion 的低干扰信息组织、以及高校数字服务平台里少见但更现代的轻量数据产品风格
- 不适合继续强化过重的拟物/粘土风格，因为该产品存在表格、图表和高频检索场景，过强装饰会伤害扫描效率与状态识别速度。

## Design Decisions

| 决策点 | 我的选择 | 理由 |
|--------|---------|------|
| 整体调性 | 校园友好 + 专业可信 + 轻量数据感 | 这是面向学生的实用工具，不该像严肃政务后台，也不能像纯营销站点，需要让用户快速理解并敢于依赖结果 |
| 色调方向 | 暖中性底 + 棕色品牌主轴 + 蓝绿功能辅助 | 项目已有稳定品牌棕色 `#884F22`，保留品牌识别；同时用蓝绿承载信息与成功状态，避免整站过于厚重 |
| 信息密度 | 中等偏紧凑 | 查询页需要快扫快点，大屏页需要装下更多指标，整体应比营销站更紧凑，但比传统后台更透气 |
| 圆角风格 | 中等圆角 `8px / 12px / 16px` | 校园产品需要一定亲和力，但表格和数据卡片仍要保持秩序感，过大圆角会降低信息效率 |
| 阴影使用 | 克制且分层明确 | 主要用于卡片、浮层和悬浮反馈，不做大面积软浮雕，避免对表格与图表阅读造成噪音 |
| 动效策略 | 功能性动效为主，装饰性动效极少 | 用户多在赶时间场景下操作，动效应服务反馈、过渡与状态切换，而不是制造视觉停顿 |
| 主要模式 | 亮色为主，支持暗色模式 | 课堂与校园环境中日间使用更常见，但夜间自习场景真实存在，暗色模式应作为完整支持而不是附属皮肤 |

## 1. Visual Theme & Atmosphere

### 设计哲学

该产品的设计应以“让学生在最短时间里做出正确判断”为第一原则：视觉上保留校园工具应有的温度和品牌记忆，但交互上遵循数据产品的秩序、可扫描性和状态清晰度。界面不是为了表现风格本身，而是为了让查询、对比、确认、回退和再次搜索都尽可能省脑力。

### 视觉关键词

- 清晰
- 可信
- 温和
- 高效
- 校园科技感

### 参考方向

- Apple Education 风格中的干净层级和轻量玻璃感
- Notion 的低干扰信息编排
- Stripe Dashboard 的数据模块组织方式
- 腾讯文档移动端工具页的高可达性

## 2. Color Palette & Roles

### 品牌主色与辅色

| Role | Token | HEX |
|------|-------|-----|
| Brand Primary | `brand-500` | `#884F22` |
| Brand Hover | `brand-600` | `#76441E` |
| Brand Active | `brand-700` | `#5F3517` |
| Brand Soft | `brand-100` | `#F3E5D8` |
| Brand Soft Hover | `brand-200` | `#E7CFBA` |
| Secondary Teal | `teal-500` | `#0F8A83` |
| Secondary Blue | `blue-500` | `#2F6FED` |
| Secondary Gold | `gold-500` | `#D39B37` |

### 中性色阶

| Token | HEX |
|-------|-----|
| `neutral-0` | `#FFFFFF` |
| `neutral-50` | `#FAF8F6` |
| `neutral-100` | `#F2EEEA` |
| `neutral-200` | `#E5DED7` |
| `neutral-300` | `#D1C7BE` |
| `neutral-400` | `#B1A396` |
| `neutral-500` | `#8A7C70` |
| `neutral-600` | `#695E55` |
| `neutral-700` | `#4C433D` |
| `neutral-800` | `#332D29` |
| `neutral-900` | `#1F1B18` |

### 语义色

| Role | Background | Foreground | Border |
|------|------------|------------|--------|
| Success | `#EAF8F3` | `#156B52` | `#A7DEC7` |
| Warning | `#FFF6E8` | `#9A5A00` | `#F3CF8D` |
| Error | `#FDEEEE` | `#B42318` | `#F5B3AE` |
| Info | `#ECF3FF` | `#1D4ED8` | `#B7CBFF` |

### 表面色层级

| Layer | Token | HEX |
|-------|-------|-----|
| Page Background | `surface-page` | `#F8F5F2` |
| Section Background | `surface-section` | `#F3EFEB` |
| Card Background | `surface-card` | `#FFFFFF` |
| Raised Card | `surface-raised` | `#FFFDFC` |
| Overlay / Popover | `surface-overlay` | `#FFFDFB` |
| Inverse Surface | `surface-inverse` | `#2A2521` |

### 暗色模式完整色板

| Token | HEX |
|-------|-----|
| `dark-bg-page` | `#141210` |
| `dark-bg-section` | `#1B1816` |
| `dark-bg-card` | `#211D1A` |
| `dark-bg-raised` | `#2A2521` |
| `dark-bg-overlay` | `#322C28` |
| `dark-text-primary` | `#F5F1EC` |
| `dark-text-secondary` | `#D0C5BA` |
| `dark-text-tertiary` | `#A59688` |
| `dark-border-subtle` | `#3D352F` |
| `dark-border-strong` | `#54493F` |
| `dark-brand-400` | `#C88F61` |
| `dark-brand-500` | `#B87948` |
| `dark-brand-600` | `#9A6035` |
| `dark-success-bg` | `#112A22` |
| `dark-success-fg` | `#7BD4AF` |
| `dark-warning-bg` | `#31230D` |
| `dark-warning-fg` | `#F5C26B` |
| `dark-error-bg` | `#331A19` |
| `dark-error-fg` | `#F3A8A1` |
| `dark-info-bg` | `#12233F` |
| `dark-info-fg` | `#9CC0FF` |

### CSS 变量命名

```css
:root {
  --color-brand-100: #F3E5D8;
  --color-brand-200: #E7CFBA;
  --color-brand-500: #884F22;
  --color-brand-600: #76441E;
  --color-brand-700: #5F3517;
  --color-teal-500: #0F8A83;
  --color-blue-500: #2F6FED;
  --color-gold-500: #D39B37;

  --color-neutral-0: #FFFFFF;
  --color-neutral-50: #FAF8F6;
  --color-neutral-100: #F2EEEA;
  --color-neutral-200: #E5DED7;
  --color-neutral-300: #D1C7BE;
  --color-neutral-400: #B1A396;
  --color-neutral-500: #8A7C70;
  --color-neutral-600: #695E55;
  --color-neutral-700: #4C433D;
  --color-neutral-800: #332D29;
  --color-neutral-900: #1F1B18;

  --color-success-bg: #EAF8F3;
  --color-success-fg: #156B52;
  --color-success-border: #A7DEC7;
  --color-warning-bg: #FFF6E8;
  --color-warning-fg: #9A5A00;
  --color-warning-border: #F3CF8D;
  --color-error-bg: #FDEEEE;
  --color-error-fg: #B42318;
  --color-error-border: #F5B3AE;
  --color-info-bg: #ECF3FF;
  --color-info-fg: #1D4ED8;
  --color-info-border: #B7CBFF;

  --color-surface-page: #F8F5F2;
  --color-surface-section: #F3EFEB;
  --color-surface-card: #FFFFFF;
  --color-surface-raised: #FFFDFC;
  --color-surface-overlay: #FFFDFB;
  --color-surface-inverse: #2A2521;

  --color-text-primary: #1F1B18;
  --color-text-secondary: #4C433D;
  --color-text-tertiary: #8A7C70;
  --color-border-subtle: #E5DED7;
  --color-border-strong: #D1C7BE;
  --color-focus-ring: #B87948;
}

[data-theme="dark"] {
  --color-surface-page: #141210;
  --color-surface-section: #1B1816;
  --color-surface-card: #211D1A;
  --color-surface-raised: #2A2521;
  --color-surface-overlay: #322C28;
  --color-surface-inverse: #F5F1EC;
  --color-text-primary: #F5F1EC;
  --color-text-secondary: #D0C5BA;
  --color-text-tertiary: #A59688;
  --color-border-subtle: #3D352F;
  --color-border-strong: #54493F;
  --color-brand-500: #B87948;
  --color-brand-600: #9A6035;
  --color-success-bg: #112A22;
  --color-success-fg: #7BD4AF;
  --color-warning-bg: #31230D;
  --color-warning-fg: #F5C26B;
  --color-error-bg: #331A19;
  --color-error-fg: #F3A8A1;
  --color-info-bg: #12233F;
  --color-info-fg: #9CC0FF;
}
```

## 3. Typography Rules

### 字体族

- 英文无衬线：`"Manrope", "Inter", "Segoe UI", "Helvetica Neue", Arial, sans-serif`
- 中文无衬线：`"PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", "Noto Sans SC", sans-serif`
- 组合正文栈：`"Manrope", "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", "Noto Sans SC", "Segoe UI", sans-serif`
- 标题栈：`"Manrope", "PingFang SC", "Noto Sans SC", sans-serif`
- 等宽栈：`"JetBrains Mono", "SFMono-Regular", "Cascadia Code", Consolas, "Liberation Mono", monospace`

### 字号层级表

| Token | Size | Weight | Line Height | Letter Spacing | Use Case |
|-------|------|--------|-------------|----------------|----------|
| `display` | `40px / 2.500rem` | 700 | `48px / 3.000rem` | `-0.03em` | 首页主标题、品牌大标题 |
| `h1` | `32px / 2.000rem` | 700 | `40px / 2.500rem` | `-0.02em` | 一级页面标题 |
| `h2` | `28px / 1.750rem` | 700 | `36px / 2.250rem` | `-0.02em` | 模块主标题 |
| `h3` | `24px / 1.500rem` | 650 | `32px / 2.000rem` | `-0.01em` | 卡片标题、图表标题 |
| `h4` | `20px / 1.250rem` | 650 | `28px / 1.750rem` | `-0.01em` | 分区标题 |
| `h5` | `18px / 1.125rem` | 600 | `26px / 1.625rem` | `-0.005em` | 表单区标题、弹窗标题 |
| `h6` | `16px / 1.000rem` | 600 | `24px / 1.500rem` | `0` | 次级标题 |
| `body-lg` | `18px / 1.125rem` | 400 | `30px / 1.875rem` | `0` | 说明性正文、首页导语 |
| `body` | `16px / 1.000rem` | 400 | `28px / 1.750rem` | `0` | 默认正文 |
| `body-sm` | `14px / 0.875rem` | 400 | `22px / 1.375rem` | `0.01em` | 表单说明、次要正文 |
| `caption` | `12px / 0.750rem` | 500 | `18px / 1.125rem` | `0.02em` | 辅助信息、图例、标签 |
| `overline` | `11px / 0.688rem` | 600 | `16px / 1.000rem` | `0.08em` | 分组标题、小型元信息 |

### 段落规则

- 正文最大宽度：`720px / 45.000rem`
- 长文阅读推荐宽度：`640px / 40.000rem`
- 正文字号行高比例：1.65 到 1.75
- 标题行高比例：1.2 到 1.35

### 中文特殊处理规则

- 中文界面默认不额外增加字距，避免稀疏感。
- 数字、节次、日期、统计值优先使用英文数字字体渲染，以强化对齐感。
- 表格中中文列名与数字值之间至少保留 `8px / 0.500rem` 视觉呼吸。
- 不在移动端使用过长标题，一行超过 14 个汉字时必须换行或缩短文案。

## 4. Component Stylings

### Button

| Variant | Size | Height | Padding | Radius | Default | Hover | Active | Disabled |
|--------|------|--------|---------|--------|---------|-------|--------|----------|
| Primary | `sm` | `32px / 2.000rem` | `0 12px / 0 0.750rem` | `8px / 0.500rem` | `bg #884F22`, `text #FFFFFF` | `bg #76441E` | `bg #5F3517`, `scale 0.99` | `bg #D1C7BE`, `text #8A7C70` |
| Primary | `md` | `40px / 2.500rem` | `0 16px / 0 1.000rem` | `10px / 0.625rem` | 同上 | 同上 | 同上 | 同上 |
| Primary | `lg` | `48px / 3.000rem` | `0 20px / 0 1.250rem` | `12px / 0.750rem` | 同上 | 同上 | 同上 | 同上 |
| Secondary | `sm/md/lg` | 同尺寸体系 | 同尺寸体系 | 同尺寸体系 | `bg #F3E5D8`, `text #5F3517`, `border #D1C7BE` | `bg #E7CFBA` | `bg #E0C3AB` | `bg #F2EEEA`, `text #8A7C70` |
| Ghost | `sm/md/lg` | 同尺寸体系 | 同尺寸体系 | 同尺寸体系 | `bg transparent`, `text #4C433D` | `bg #F2EEEA` | `bg #E5DED7` | `text #B1A396` |
| Danger | `sm/md/lg` | 同尺寸体系 | 同尺寸体系 | 同尺寸体系 | `bg #D92D20`, `text #FFFFFF` | `bg #B42318` | `bg #912018` | `bg #F5B3AE`, `text #FFFFFF` |
| Link | `sm/md/lg` | auto | `0` | `0` | `text #2F6FED` | `text #1D4ED8`, `underline` | `text #163EAF` | `text #B7CBFF` |

- Focus ring：统一 `0 0 0 3px rgba(184, 121, 72, 0.24)`
- 图标按钮默认方形，尺寸与高度一致
- 首页主 CTA 可使用轻微纵向位移 `translateY(-1px)`，其他按钮不建议使用夸张浮动

### Input / TextArea / Select

| Component | State | Background | Border | Text | Shadow |
|-----------|-------|------------|--------|------|--------|
| Input | Default | `#FFFFFF` | `#D1C7BE` | `#1F1B18` | none |
| Input | Hover | `#FFFFFF` | `#B1A396` | `#1F1B18` | none |
| Input | Focus | `#FFFFFF` | `#B87948` | `#1F1B18` | `0 0 0 3px rgba(184, 121, 72, 0.18)` |
| Input | Error | `#FFF7F7` | `#D92D20` | `#1F1B18` | `0 0 0 3px rgba(217, 45, 32, 0.12)` |
| Input | Disabled | `#F2EEEA` | `#E5DED7` | `#8A7C70` | none |
| TextArea | Default | `#FFFFFF` | `#D1C7BE` | `#1F1B18` | none |
| Select | Default | `#FFFFFF` | `#D1C7BE` | `#1F1B18` | none |

- 高度：`40px / 2.500rem`
- 大尺寸高度：`48px / 3.000rem`
- 内边距：`12px 14px / 0.750rem 0.875rem`
- 圆角：`10px / 0.625rem`
- 占位符：`#8A7C70`
- 下拉箭头区与输入区之间至少留 `32px / 2.000rem`

### Card

| Type | Radius | Border | Shadow | Padding | Interaction |
|------|--------|--------|--------|---------|-------------|
| Standard Card | `16px / 1.000rem` | `1px solid #E5DED7` | `shadow-1` | `20px / 1.250rem` | 默认静态 |
| Elevated Card | `16px / 1.000rem` | `1px solid rgba(209,199,190,0.72)` | `shadow-2` | `24px / 1.500rem` | Hover 时提升为 `shadow-3` |
| Analytics Card | `16px / 1.000rem` | `1px solid #E5DED7` | `shadow-1` | `24px / 1.500rem` | 可带顶部色条或指标图标 |

- 卡片优先使用边框分层，不使用大面积重阴影
- 查询页结果卡允许悬浮背景微变，但不建议整卡大幅抬升

### Table

| Item | Spec |
|------|------|
| Header Height | `44px / 2.750rem` |
| Row Height | `48px / 3.000rem` |
| Dense Row Height | `40px / 2.500rem` |
| Header Background | `#F3EFEB` |
| Row Background | `#FFFFFF` |
| Zebra Stripe | `#FCFAF8` |
| Hover Row | `#F8F1EB` |
| Border Color | `#E5DED7` |
| Sticky Column Background | `#FFFDFC` |

- 全日状态矩阵表应支持固定首列、横向滚动、表头吸附
- 状态单元格不依赖 emoji 作为唯一编码，必须同时保留色块或 token 映射文本的能力

### Navigation

| Component | Spec |
|-----------|------|
| Top Bar Height | `64px / 4.000rem` mobile, `72px / 4.500rem` desktop |
| Top Bar Background | `rgba(255,253,251,0.88)` + blur |
| Active Item | `text #884F22`, `indicator bg #F3E5D8` |
| Hover Item | `bg #F3EFEB`, `text #1F1B18` |
| Back Button | `40px / 2.500rem` square, border subtle |
| Breadcrumb | 仅桌面与大屏页面使用，文字 `12px / 0.750rem` |

- 本项目不建议上侧栏
- 查询页采用顶部返回式导航
- Dashboard 可在桌面增加页内二级锚点标签，但不做复杂 tabs + sidebar 叠加

### Modal / Drawer

| Component | Width | Radius | Mask | Animation |
|-----------|-------|--------|------|----------|
| Modal S | `360px / 22.500rem` | `20px / 1.250rem` | `rgba(31,27,24,0.48)` | `fade + scale(0.98->1)` `180ms` |
| Modal M | `480px / 30.000rem` | `20px / 1.250rem` | 同上 | 同上 |
| Drawer Mobile | `100vw` | `20px 20px 0 0 / 1.250rem 1.250rem 0 0` | 同上 | `translateY(12px->0)` `220ms` |
| Drawer Desktop | `420px / 26.250rem` | `20px / 1.250rem` | 同上 | `translateX(16px->0)` `220ms` |

- 确认类对话框优先用 Modal
- 辅助配置或历史筛选可用 Drawer

### Toast / Alert / Badge

| Component | Spec |
|-----------|------|
| Toast Position | mobile 顶部居中；desktop 右上角 |
| Toast Duration | 成功 `2400ms`，信息 `3200ms`，错误手动关闭或 `5000ms` |
| Toast Radius | `12px / 0.750rem` |
| Alert Style | 实色边框 + 淡底，左侧图标列固定 `20px / 1.250rem` |
| Badge Default | `height 20px / 1.250rem`, `padding 0 8px / 0 0.500rem`, `radius 999px / 62.438rem` |

### Tag / Chip

| Variant | Background | Foreground | Border |
|--------|------------|------------|--------|
| Brand | `#F3E5D8` | `#5F3517` | `#E7CFBA` |
| Info | `#ECF3FF` | `#1D4ED8` | `#B7CBFF` |
| Success | `#EAF8F3` | `#156B52` | `#A7DEC7` |
| Warning | `#FFF6E8` | `#9A5A00` | `#F3CF8D` |
| Neutral | `#F2EEEA` | `#4C433D` | `#E5DED7` |

- 可关闭 Chip 关闭按钮尺寸：`16px / 1.000rem`
- 热搜项建议使用 Brand 或 Neutral，不用彩色混搭

### Tabs

| Item | Spec |
|------|------|
| Container | `height 40px / 2.500rem`, `bg #F3EFEB` |
| Tab Radius | `8px / 0.500rem` |
| Active Tab | `bg #FFFFFF`, `text #884F22`, subtle shadow |
| Hover Tab | `text #1F1B18` |
| Indicator | 胶囊底或 `2px / 0.125rem` 下划线，二选一，禁止混用 |

- 时间范围切换建议统一重构为 Tabs token，不再手写样式

### Pagination

| Item | Spec |
|------|------|
| Height | `36px / 2.250rem` |
| Min Width | `36px / 2.250rem` |
| Radius | `8px / 0.500rem` |
| Current | `bg #884F22`, `text #FFFFFF` |
| Default | `bg #FFFFFF`, `border #D1C7BE`, `text #4C433D` |

- 当前项目暂未使用传统分页，但“加载更多”按钮应复用 Pagination/Load More 规范

### Empty State / Loading State

| Component | Spec |
|-----------|------|
| Empty Illustration Size | `64px / 4.000rem` to `96px / 6.000rem` |
| Empty Title | `16px / 1.000rem`, 600 |
| Empty Description | `14px / 0.875rem`, secondary text |
| Loading Spinner | `20px / 1.250rem` inline, `32px / 2.000rem` block |
| Skeleton Radius | `8px / 0.500rem` |

- 查询结果为空时必须附带可行动文案，例如“试试更换节次或教学楼名称”
- Loading 不要继续使用厚重拟物圆球，应改为更轻的品牌色环形或骨架屏

## 5. Layout Principles

### 基础间距单元

- 采用 `4px` 基础单位，主布局按 `8px` 递增组织

### 间距阶梯表

| Token | Value |
|-------|-------|
| `space-1` | `4px / 0.250rem` |
| `space-2` | `8px / 0.500rem` |
| `space-3` | `12px / 0.750rem` |
| `space-4` | `16px / 1.000rem` |
| `space-5` | `20px / 1.250rem` |
| `space-6` | `24px / 1.500rem` |
| `space-8` | `32px / 2.000rem` |
| `space-10` | `40px / 2.500rem` |
| `space-12` | `48px / 3.000rem` |
| `space-16` | `64px / 4.000rem` |

### 页面布局

| Item | Mobile | Tablet | Desktop | Wide |
|------|--------|--------|---------|------|
| Page Padding | `16px / 1.000rem` | `24px / 1.500rem` | `32px / 2.000rem` | `40px / 2.500rem` |
| Content Max Width | `100%` | `720px / 45.000rem` | `1120px / 70.000rem` | `1280px / 80.000rem` |
| Query Form Width | `100%` | `640px / 40.000rem` | `720px / 45.000rem` | `720px / 45.000rem` |
| Dashboard Width | `100%` | `960px / 60.000rem` | `1120px / 70.000rem` | `1280px / 80.000rem` |

### 网格系统

- Mobile：4 列，gutter `16px / 1.000rem`
- Tablet：8 列，gutter `20px / 1.250rem`
- Desktop：12 列，gutter `24px / 1.500rem`
- 查询页结果卡片建议：
  - mobile `2` 列或 `3` 列，视教室名长度而定
  - tablet `3-4` 列
  - desktop `4-6` 列

## 6. Depth & Elevation

### 阴影定义

| Level | CSS box-shadow | Use Case |
|-------|----------------|----------|
| `shadow-1` | `0 1px 2px rgba(31,27,24,0.06), 0 4px 12px rgba(31,27,24,0.04)` | 默认卡片 |
| `shadow-2` | `0 6px 20px rgba(31,27,24,0.08), 0 2px 6px rgba(31,27,24,0.05)` | 悬浮卡、下拉菜单 |
| `shadow-3` | `0 10px 30px rgba(31,27,24,0.12), 0 4px 10px rgba(31,27,24,0.06)` | Modal、Drawer |
| `shadow-4` | `0 16px 48px rgba(31,27,24,0.16), 0 8px 20px rgba(31,27,24,0.10)` | 全局浮层、重点覆盖层 |

### 使用场景

- 页面级容器：尽量无阴影，仅靠背景分层
- 内容卡片：`shadow-1`
- Hover / Popover：`shadow-2`
- Modal / Drawer：`shadow-3`
- 不建议使用 inset 拟物阴影作为主系统风格

### z-index 规范表

| Layer | z-index |
|-------|---------|
| Base Content | `0` |
| Sticky Table Column | `10` |
| Sticky Header | `20` |
| Dropdown / Popover | `40` |
| Toast | `50` |
| Overlay Mask | `60` |
| Modal / Drawer | `70` |
| Global Emergency Notice | `80` |

## 7. Design Do's and Don'ts

### 不要做

- 不要再使用大面积厚重软浮雕、强内阴影和多层高光作为默认组件样式。
- 不要仅靠 emoji 区分教室状态，状态必须有色彩与文本的双重表达。
- 不要把首页做成营销海报式长页面，本项目核心是快速进入查询。
- 不要在查询页使用过大的留白和过长动效，影响高频操作效率。
- 不要在同一页面同时混用超过 1 种主交互按钮形态。
- 不要用超过 4 种高饱和彩色卡片并列展示指标，尤其是在大屏页。
- 不要把表格做成纯视觉装饰组件，首列、表头、滚动提示必须可用。
- 不要让错误提示只写“失败”，必须说明可执行下一步。
- 不要在移动端把热搜、历史记录、日期选择都放进同一个视觉层级里争抢注意力。
- 不要让二维码、公告、推广信息压过核心查询路径。

### 要做

- 要优先保证搜索框、日期选择、节次选择和提交按钮形成清晰的操作主路径。
- 要把系统状态、权限状态、教学周历提醒作为页面顶部的一级反馈。
- 要在全天状态页强化首列固定、状态图例和横向滚动可发现性。
- 要在大屏页统一指标卡、图表标题、时间范围切换的视觉秩序。
- 要让所有组件在亮色与暗色模式下都保持相同的信息层级和交互反馈。

### 针对本项目的设计护栏

- 品牌主色必须围绕 `#884F22` 构建，不能改成蓝紫科技风主色。
- 查询功能优先级必须高于公告、推广和内容运营模块。
- Dashboard 的图表色板必须收敛，品牌棕只做主线，不可每张图都抢主色。
- 楼宇、节次、日期、周次属于高价值信息，必须优先使用高对比文本。
- 任何状态提醒组件都要兼顾“权限问题”和“教学周历外”这两类真实业务异常。

## 8. Responsive Behavior

### 断点定义

| Breakpoint | Value |
|------------|-------|
| `mobile` | `0px - 767px / 0rem - 47.938rem` |
| `tablet` | `768px - 1023px / 48.000rem - 63.938rem` |
| `desktop` | `1024px - 1439px / 64.000rem - 89.938rem` |
| `wide` | `1440px+ / 90.000rem+` |

### 各断点布局变化

- Mobile：
  - 单列主布局
  - 查询表单纵向堆叠
  - 全日状态表横向滚动，首列固定
  - Dashboard 指标卡 2 列或 3 列网格
- Tablet：
  - 首页卡片可双列
  - 查询表单部分字段可并排
  - Dashboard 图表开始采用 2 列组合
- Desktop：
  - 首页入口卡片可 2 到 3 列
  - Dashboard 完整采用模块化卡片矩阵
  - 全日状态页图例、筛选区与结果区可形成更稳定的层级
- Wide：
  - 扩大图表区域和表格可视宽度
  - 保持内容容器最大宽度，不无限拉伸文本

### 触摸目标最小尺寸

- 按钮、图标按钮、分页项、分段控件：至少 `44px / 2.750rem`
- 热搜 Chip、可关闭 Tag：至少 `36px / 2.250rem` 高
- 表格滚动区外的可点击元素禁止低于 `40px / 2.500rem`

### 导航折叠策略

- Mobile：顶部栏保留返回与标题，不展示面包屑
- Tablet：可在 Dashboard 增加简化页内 tabs
- Desktop：支持面包屑或二级说明，但依旧不启用左侧导航

## 9. Agent Prompt Guide

### 核心色彩速查表

| Purpose | HEX |
|---------|-----|
| Brand | `#884F22` |
| Brand Hover | `#76441E` |
| Brand Active | `#5F3517` |
| Surface Page | `#F8F5F2` |
| Surface Card | `#FFFFFF` |
| Text Primary | `#1F1B18` |
| Text Secondary | `#4C433D` |
| Border | `#E5DED7` |
| Success | `#156B52` |
| Warning | `#9A5A00` |
| Error | `#B42318` |
| Info | `#1D4ED8` |

### CSS 变量完整列表

```css
--color-brand-100
--color-brand-200
--color-brand-500
--color-brand-600
--color-brand-700
--color-teal-500
--color-blue-500
--color-gold-500
--color-neutral-0
--color-neutral-50
--color-neutral-100
--color-neutral-200
--color-neutral-300
--color-neutral-400
--color-neutral-500
--color-neutral-600
--color-neutral-700
--color-neutral-800
--color-neutral-900
--color-success-bg
--color-success-fg
--color-success-border
--color-warning-bg
--color-warning-fg
--color-warning-border
--color-error-bg
--color-error-fg
--color-error-border
--color-info-bg
--color-info-fg
--color-info-border
--color-surface-page
--color-surface-section
--color-surface-card
--color-surface-raised
--color-surface-overlay
--color-surface-inverse
--color-text-primary
--color-text-secondary
--color-text-tertiary
--color-border-subtle
--color-border-strong
--color-focus-ring
```

### Vant 4 主题变量覆盖

| Vant Variable | Value |
|---------------|-------|
| `--van-primary-color` | `#884F22` |
| `--van-primary-color-light` | `#F3E5D8` |
| `--van-primary-color-dark` | `#5F3517` |
| `--van-success-color` | `#156B52` |
| `--van-warning-color` | `#9A5A00` |
| `--van-danger-color` | `#B42318` |
| `--van-text-color` | `#1F1B18` |
| `--van-text-color-2` | `#4C433D` |
| `--van-text-color-3` | `#8A7C70` |
| `--van-background` | `#F8F5F2` |
| `--van-background-2` | `#FFFFFF` |
| `--van-background-3` | `#F3EFEB` |
| `--van-border-color` | `#E5DED7` |

### 自定义 CSS 变量（补充 Vant 未覆盖的场景）

| Token | CSS Variable |
|-------|--------------|
| 品牌色 | `var(--color-brand-500)` |
| 品牌浅色 | `var(--color-brand-100)` |
| 页面背景 | `var(--color-surface-page)` |
| 卡片背景 | `var(--color-surface-card)` |
| 主文字 | `var(--color-text-primary)` |
| 次文字 | `var(--color-text-secondary)` |
| 辅助文字 | `var(--color-text-tertiary)` |
| 细边框 | `var(--color-border-subtle)` |
| 成功背景 | `var(--color-success-bg)` |
| 成功前景 | `var(--color-success-fg)` |
| 警告背景 | `var(--color-warning-bg)` |
| 警告前景 | `var(--color-warning-fg)` |
| 错误背景 | `var(--color-error-bg)` |
| 错误前景 | `var(--color-error-fg)` |
| 信息背景 | `var(--color-info-bg)` |
| 信息前景 | `var(--color-info-fg)` |

### 给 AI 的快速提示词模板

```text
为 QFNU 教室查询系统生成界面时，请遵循以下约束：
1. 保留品牌主色 #884F22，不要改成紫色或霓虹科技风。
2. 风格定位为校园友好、专业可信、轻量数据感。
3. 使用 Vant 4 组件库作为基础 UI 框架，通过 CSS 变量覆盖主题色。
4. 优先保证查询效率与信息可扫描性，不要使用厚重拟物和大面积软浮雕。
5. 查询页使用中等偏紧凑布局；首页突出功能入口；Dashboard 保持数据卡和图表秩序。
6. 组件使用 Vant 默认圆角体系，阴影克制，边框清晰。
7. 状态信息必须同时用颜色和文字表达，不能只依赖 emoji。
8. 移动端优先，确保搜索、日期、节次、结果列表和横向表格都可用。
9. 不再使用 Tailwind CSS，所有样式通过 Vant 主题变量 + scoped CSS 实现。
```
