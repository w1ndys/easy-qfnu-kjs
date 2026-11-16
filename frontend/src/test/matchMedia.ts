// 可控的 matchMedia 垫片（tasklist 第 10 项，仅供测试）。
//
// jsdom 不实现 matchMedia，antd 的响应式组件与 useReducedMotion 都依赖它。
// 之前 setup.ts 里是一个 matches 恒为 false 的桩；第 10 项要验证
// 「prefers-reduced-motion 生效」与「偏好变化后实时跟随」，所以必须能改匹配值
// 并触发 change 事件。这里按 media query 字符串登记状态，默认一律不匹配，
// 用例用 setMediaQueryMatches 显式打开/关闭。

type ChangeListener = (event: MediaQueryListEvent) => void;

interface MediaQueryEntry {
  matches: boolean;
  listeners: Set<ChangeListener>;
}

const entries = new Map<string, MediaQueryEntry>();

/** 取（或建立）一个 media query 的状态；同一个查询字符串的多个 MediaQueryList 共享它。 */
function entry(query: string): MediaQueryEntry {
  const existing = entries.get(query);
  if (existing !== undefined) {
    return existing;
  }
  const created: MediaQueryEntry = { matches: false, listeners: new Set() };
  entries.set(query, created);
  return created;
}

/** 造一个 MediaQueryList：matches 是 getter，读的是登记表里的当前值。 */
function createMediaQueryList(query: string): MediaQueryList {
  const state = entry(query);

  const list = {
    media: query,
    onchange: null,
    get matches() {
      return state.matches;
    },
    addEventListener: (
      _type: string,
      listener: EventListenerOrEventListenerObject | null,
    ) => {
      if (typeof listener === "function") {
        state.listeners.add(listener as ChangeListener);
      }
    },
    removeEventListener: (
      _type: string,
      listener: EventListenerOrEventListenerObject | null,
    ) => {
      if (typeof listener === "function") {
        state.listeners.delete(listener as ChangeListener);
      }
    },
    // 旧版 Safari 的接口：useReducedMotion 在 addEventListener 缺失时会回退到它们。
    addListener: (listener: ChangeListener) => {
      state.listeners.add(listener);
    },
    removeListener: (listener: ChangeListener) => {
      state.listeners.delete(listener);
    },
    dispatchEvent: () => true,
  };

  return list as unknown as MediaQueryList;
}

/**
 * 安装垫片。只在 window.matchMedia 不存在时安装（jsdom 就是这种情况），
 * 已有实现的环境不覆盖，避免把真实浏览器行为换掉。
 */
export function installMatchMedia(): void {
  if (typeof window === "undefined") {
    return;
  }
  if (typeof window.matchMedia === "function") {
    return;
  }
  Object.defineProperty(window, "matchMedia", {
    writable: true,
    configurable: true,
    value: createMediaQueryList,
  });
}

/** 改某个 media query 的匹配值，并同步通知所有监听者（模拟系统设置变化）。 */
export function setMediaQueryMatches(query: string, matches: boolean): void {
  const state = entry(query);
  state.matches = matches;

  const event = { matches, media: query } as MediaQueryListEvent;
  for (const listener of [...state.listeners]) {
    listener(event);
  }
}

/** 每个用例后复位：匹配值归零、监听者清空，避免跨文件/跨用例互相污染。 */
export function resetMediaQueries(): void {
  for (const state of entries.values()) {
    state.matches = false;
    state.listeners.clear();
  }
  entries.clear();
}
