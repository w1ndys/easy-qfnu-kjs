import { describe, expect, it } from "vitest";
import {
  computeScale,
  computeScreenGeometry,
  SCREEN_HEIGHT,
  SCREEN_WIDTH,
} from "./scale";

// 大屏缩放系数（design D6、正确性属性 8）：窗口只改 scale，不改内层布局。
// 三档验收视口是任务原文给的 1920×1080 / 2560×1440 / 1280×1024，
// 另补「更宽」「更高」两个非 16:9 方向与非法输入。
describe("computeScale", () => {
  it("16:9 视口 1:1 铺满", () => {
    expect(computeScale(1920, 1080)).toBe(1);
  });

  it("16:9 放大到 2560×1440", () => {
    expect(computeScale(2560, 1440)).toBeCloseTo(4 / 3, 10);
  });

  it("4:3 视口 1280×1024 按宽度受限缩到 2/3，上下留边", () => {
    // min(1280/1920, 1024/1080) = min(0.6667, 0.9481) = 0.6667
    expect(computeScale(1280, 1024)).toBeCloseTo(2 / 3, 10);
  });

  it("更宽的视口按高度受限（留左右黑边）", () => {
    expect(computeScale(3840, 1080)).toBe(1);
  });

  it("更高的视口按宽度受限（留上下黑边）", () => {
    expect(computeScale(1920, 2160)).toBe(1);
  });

  it("尺寸不可用时退化为 1:1，而不是 0 或 NaN", () => {
    expect(computeScale(0, 0)).toBe(1);
    expect(computeScale(-100, 1080)).toBe(1);
    expect(computeScale(Number.NaN, 1080)).toBe(1);
    expect(computeScale(Number.POSITIVE_INFINITY, 1080)).toBe(1);
  });
});

describe("computeScreenGeometry", () => {
  it("16:9 视口不留边", () => {
    expect(computeScreenGeometry(1920, 1080)).toEqual({
      scale: 1,
      offsetX: 0,
      offsetY: 0,
    });
    const larger = computeScreenGeometry(2560, 1440);
    expect(larger.scale).toBeCloseTo(4 / 3, 10);
    expect(larger.offsetX).toBeCloseTo(0, 10);
    expect(larger.offsetY).toBeCloseTo(0, 10);
  });

  it("1280×1024 左右无留边、上下各留 152px", () => {
    const geometry = computeScreenGeometry(1280, 1024);
    expect(geometry.scale).toBeCloseTo(2 / 3, 10);
    expect(geometry.offsetX).toBeCloseTo(0, 10);
    expect(geometry.offsetY).toBeCloseTo(152, 10);
  });

  it("超宽视口左右均分留边", () => {
    const geometry = computeScreenGeometry(3840, 1080);
    expect(geometry.scale).toBe(1);
    expect(geometry.offsetX).toBeCloseTo(960, 10);
    expect(geometry.offsetY).toBeCloseTo(0, 10);
  });

  it("尺寸不可用时留边为 0，不产生负偏移", () => {
    expect(computeScreenGeometry(0, 0)).toEqual({
      scale: 1,
      offsetX: 0,
      offsetY: 0,
    });
  });

  it("任意合法视口下缩放后的画布都不超出视口（不裁切）", () => {
    const viewports: ReadonlyArray<readonly [number, number]> = [
      [320, 568],
      [600, 1024],
      [1024, 768],
      [1280, 1024],
      [1440, 900],
      [1920, 1080],
      [2560, 1440],
      [3840, 2160],
      [1920, 2160],
      [3840, 1080],
    ];

    for (const [width, height] of viewports) {
      const { scale, offsetX, offsetY } = computeScreenGeometry(width, height);
      expect(scale).toBeGreaterThan(0);
      expect(offsetX).toBeGreaterThanOrEqual(0);
      expect(offsetY).toBeGreaterThanOrEqual(0);
      expect(offsetX * 2 + SCREEN_WIDTH * scale).toBeCloseTo(width, 8);
      expect(offsetY * 2 + SCREEN_HEIGHT * scale).toBeCloseTo(height, 8);
    }
  });
});
