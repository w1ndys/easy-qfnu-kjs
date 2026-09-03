#!/usr/bin/env python3
"""easy-qfnu-kjs 本地验证码识别（OCR_CMD 参考实现）。

用法：
    ocr_ddddocr.py [图片路径]      # 读取 argv[1] 图片文件
    cat img.png | ocr_ddddocr.py  # 或从 stdin 读取图片字节

行为：使用 ddddocr 识别，识别文本输出到 stdout（首行）。
未安装 ddddocr 时向 stderr 输出清晰错误并以非零码退出。
"""
import sys


def main() -> int:
    try:
        import ddddocr
    except ImportError:
        print("缺少依赖 ddddocr：请先安装（pip install ddddocr）或改设 OCR_CMD。",
              file=sys.stderr)
        return 2

    if len(sys.argv) > 1:
        path = sys.argv[1]
        with open(path, "rb") as f:
            data = f.read()
    else:
        data = sys.stdin.buffer.read()
    if not data:
        print("未读取到图片字节（缺少参数或 stdin 为空）。", file=sys.stderr)
        return 2

    try:
        ocr = ddddocr.DdddOcr(show_ad=False)
    except TypeError:
        ocr = ddddocr.DdddOcr()

    text = ocr.classification(data)
    sys.stdout.write(text)
    sys.stdout.flush()
    return 0


if __name__ == "__main__":
    sys.exit(main())
