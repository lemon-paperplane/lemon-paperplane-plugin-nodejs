# Lemon Paperplane Node.js 插件

为柠檬纸飞机提供 Node.js 支持：

- 工具链 `node`：发现 nvm / fnm / volta / n / asdf / mise / nodenv / nvs / Homebrew / 系统安装 / nvm-windows / scoop，按 `index.json` 下载（镜像优先，sha256 校验，老版本在 Apple Silicon 上退回 x64）。
- 识别：`package.json`（跳过 `node_modules`、`uni_modules` 等目录里的包），认领 workspace 成员（`workspaces`、`pnpm-workspace.yaml`、`lerna.json`）。
- Node 版本：`.nvmrc` / `.node-version` / `.tool-versions` / `mise.toml` / `volta.node`（支持 `lts/*`、`lts/<代号>` 等别名）→ `engines.node` → 依赖经验规则（Vite、Next、Nuxt、Vue CLI、react-scripts、webpack 4、uni-app Vue2、node-sass）→ 依赖自己声明的 `engines`，求交后化简成一个约束。
- 包管理器：`packageManager` 字段 → 锁文件（多个时看 node_modules 里最近一次安装的记录）→ `.yarnrc.yml` → `engines`。yarn / pnpm 不在时依次用 corepack、已有安装、或装到插件数据目录。
- 依赖：状态检查（缺目录、缺包、锁文件比安装新、`.yarn-integrity` 与 package.json 不一致），安装不改锁文件。
- 运行配置 `npm-script`，诊断规则，URL / 就绪识别，「依赖」标签页，MCP 工具 `nodejs.scripts`。

## 框架插件约定

框架插件可以在 detect 返回的 facet `Data` 里给出脚本提示，nodejs 在 analyze 时读取：

| 键 | 类型 | 作用 |
|---|---|---|
| `preferScripts` | `[]string` | 默认脚本候选，按顺序 |
| `scriptTitles` | `map[string]string` | 脚本显示名 |
| `hideScripts` | `[]string` | 不生成运行配置的脚本 |

模型（`Module.Models["nodejs"]`）里有 `versions`（直接依赖实际版本）、`node`（版本需求）、`legacyPackage`（需要 OpenSSL 旧版兼容的依赖）等字段，见 `plugin/plugin.go` 的 `Model`。

## 构建与测试

```sh
./build.sh
go test ./...
```
