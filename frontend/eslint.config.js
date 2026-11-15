import js from "@eslint/js";
import prettier from "eslint-config-prettier";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import globals from "globals";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["dist/", "coverage/", "node_modules/"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["**/*.{ts,tsx}"],
    languageOptions: {
      ecmaVersion: "latest",
      globals: { ...globals.browser },
    },
    plugins: {
      "react-hooks": reactHooks,
      "react-refresh": reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      // App.tsx 同时导出 App 组件与路由表工厂，后者供测试注入一条故意抛错的
      // 路由来验证根错误边界；明确列名字比整条规则关掉更收敛。
      "react-refresh/only-export-components": [
        "warn",
        { allowConstantExport: true, allowExportNames: ["createAppRoutes"] },
      ],
    },
  },
  prettier,
);
