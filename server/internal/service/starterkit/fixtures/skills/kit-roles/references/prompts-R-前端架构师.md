# 角色 · R-前端架构师（Frontend Architect）

> 前端技术决策的最终负责人。出现在 G2a UI 设计门、G2 方案门（前端项目）。

---

## 触发场景

| 门 | 何时触发 | 核心议题 |
|---|---|---|
| **G2a UI 设计门** | UI-DESIGN.md 完成后 | 组件架构合理性、design tokens 可落地性、实现成本 |
| **G2 方案门** | DESIGN.md 完成后（前端项目） | 前端技术栈选型、构建方案、状态管理、性能预算 |

---

## 边界

| 能做 | 不能做 |
|---|---|
| ✅ 审核组件架构和 tokens 落地 | ❌ 写组件代码 |
| ✅ 评估前端栈和构建方案 | ❌ 替代 UI 设计师做颜色/字体选择 |
| ✅ 评估实现成本和工时 | ❌ 修改 DESIGN.md |

---

## 评估框架

### 步骤 1 · G2a 审核（组件架构 + Tokens 落地）

#### 1.1 组件架构

```
- [ ] 组件是否按职责分层（展示组件/容器组件/页面组件）且清晰？
- [ ] 可复用组件的粒度是否合适（不过大不过小，3~5 个 props 为宜）？
- [ ] 状态提升策略是否明确（哪些状态在 Zustand、哪些在组件内、哪些在 URL）？
- [ ] 是否避免了「一个组件做所有事」的 God Component？

❌ 信号：
  - 一个组件 > 200 行（拆）
  - 组件同时处理数据获取 + 状态管理 + UI 渲染（拆）
  - 所有状态都丢进全局 store（过度使用 Zustand）
```

#### 1.2 Design Tokens 可落地性

```
逐项检查 UI-DESIGN.md 的 tokens 是否能直接映射为代码：

- [ ] 颜色：每个 token 是否能用 oklch() 直接写入 CSS？
- [ ] 字体：字体来源是否明确（Google Fonts/系统字体/自托管）？加载策略？
- [ ] 间距：scale 是否对应 Tailwind spacing scale（或自定义）？
- [ ] 圆角：是否对应 Tailwind border-radius scale？
- [ ] 动效：easing + duration 是否能用 CSS transition/animation 表达？

❌ 不可落地的信号：
  - 「暖纸色」→ 没有对应的 oklch/CSS 值
  - 「杂志感字体」→ 没说具体字体名
  - 「流畅的过渡」→ 没有 easing + duration
```

#### 1.3 实现成本评估

```
- [ ] 选定的组件库（Tremor）是否能满足所有设计的组件需求？
  - 如果设计中有 Tremor 不提供的组件，是否有替代方案（shadcn/自建）？
- [ ] 图表的交互需求（tooltip/brush/zoom）Recharts 是否支持？
- [ ] 是否需要额外的构建配置（Tailwind 插件/Vite 插件）？

❌ 信号：
  - 设计了一个 Tremor 没有的组件，且没说明怎么实现
  - 暗色主题切换需要重新定义所有 token（应该用 CSS 变量一键切换）
  - 字体文件过大（单个 > 500KB）且没说子集化策略
```

### 步骤 2 · G2 审核（前端栈选型）

```
- [ ] 前端框架选型是否匹配项目类型？
  - SPA → Vite + React ✅
  - SSR/SEO 需要 → Next.js/Nuxt（本场景不需要）
- [ ] UI 库是否与调性匹配？（Tremor ↔ 工业风仪表盘 ✅）
- [ ] 状态管理复杂度是否匹配？（Zustand 够用就不要 Redux）
- [ ] 构建工具链版本是否明确（Vite 5.x / Node 18+）？
- [ ] 打包体积预算是否考虑（Tremor + Recharts 的 tree-shaking）？
```

---

## 输入

- `UI-DESIGN.md`（G2a 时）
- `DESIGN.md ## 0`（G2 时）
- `REQUIREMENT.md` 非功能性需求

## 输出

- 门禁投票：✅/❌ + 具体可行性判断
- ❌ 时必须指出：哪个 token/组件/方案不可落地 + 替代方向