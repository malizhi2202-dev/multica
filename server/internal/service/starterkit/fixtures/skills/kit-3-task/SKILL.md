---
name: kit-3-task
description: "阶段 3 · TASK 拆解：拆可独立验证的任务、read_files/write_files 边界、可执行 verify 命令、依赖图与波次、逐 task 自动化投票。含 Task 门 4 专家投票协议。"
---
# 阶段 3 · TASK — 把设计拆成可并行的原子任务

## 角色

你是 Planner。

## 输入

- `@.specs/<change-id>/REQUIREMENT.md`
- `@.specs/<change-id>/DESIGN.md`（**必读 `## 0. 技术栈选定`**——任务的 verify 命令、依赖管理、目录结构必须按选定的栈写）
- `@.specs/CONTEXT.md`

## 入口门禁（Artifact Preflight）

开始拆任务前先检查上游工件，缺任何一项都不要继续：

- 缺 `REQUIREMENT.md`：停止，回到 `@code-kit/prompts/1-requirement.md`。
- 缺 `DESIGN.md`：停止，回到 `@code-kit/prompts/2-design.md`；MVP 模式也必须生成 `DESIGN-lite`。
- 前端 / UI 项目缺 `UI-DESIGN.md`：停止，回到 `@code-kit/prompts/2a-ui-design.md`。纯后端 / CLI / lib 项目才可跳过。
- 禁止 Planner 自己脑补技术栈、触碰模块、禁动清单或 `write_files` 边界。

触发时输出：

```text
规则 R2.7 触发：3-task 缺少 <工件>。本次先回到 <阶段> 补齐，不能直接拆任务。
```

## 你的职责

使用 `@code-kit/templates/TASK.md` 模板产出**原子任务列表**。

### 拆解原则

1. **大小**：一个任务在 fresh context 下 2~10 分钟可完成
2. **粒度**：按文件冲突切，不按层切。优先「垂直切片」（一个特性贯穿模型/API/UI）而非「水平层」（先所有模型再所有 API）
3. **并行标记 `[P]`**：互不冲突的任务标 `[P]`，会成为同一个执行波次
4. **依赖**：每个任务显式声明 `depends_on: <task-id>`
5. **每任务必备字段**：
   - `id` —— 形如 `T01`、`T02-1`
   - `name` —— 一句话
   - `read_files` —— **参考边界**：AI 在这个任务中允许 read 的文件（支持 glob，比如 `src/repos/*`、`src/utils/date.ts`）
   - `write_files` —— **修改边界**：AI 可以创建 / 修改 / 删除的文件。**超出这个范围的 diff 会被提交前的 R6.5 边界 verify 拦住**
   - `action` —— 要做什么（不写代码，写意图）
   - `verify` —— 一条可执行的验证命令（如 `npm test -- theme.test.ts`、`curl ... | jq ...`）
   - `done` —— 完成判定（一句话，对应 AC 的某个子项）

#### `read_files` 与 `write_files` 的区别（B3 老项目护栏）

- **`read_files` 应该包含**：
  - 本任务要修改的文件（= write_files 的超集）
  - 本任务要 import / 参考的既有模块（沿用抽象要 read 才能用）
  - DESIGN.md `## 0.5.1` 「触碰模块」中的「已有·复用」项

- **`write_files` 严格控制**：
  - DESIGN.md `## 0.5.1` 「新增模块」项都加进来
  - DESIGN.md `## 0.5.1` 「触碰模块」中需要修改的那些
  - **不允许加「禁动清单」里的文件**（这是 R7.3 + R6.5 联动拦截点）

- **示例**：
  ```xml
  <read_files>
    src/features/notifications/*
    src/lib/api-client.ts       <!-- 沿用 -->
    src/components/Modal.tsx    <!-- 复用 -->
    src/utils/date.ts           <!-- 沿用 -->
  </read_files>
  <write_files>
    src/features/notifications/NotificationCenter.tsx
    src/features/notifications/useNotifications.ts
    src/features/notifications/__tests__/*
  </write_files>
  ```

### 波次划分

把任务按依赖图分层：
- 同层 = 同波次（并行执行）
- 跨层 = 顺序执行

输出形如：

```
Wave 1 (parallel): T01[P], T02[P]
Wave 2 (parallel): T03[P], T04[P] (depends on T01)
Wave 3:            T05 (depends on T03, T04)
```

### 任务模板（XML，便于 AI 解析与执行）

```xml
<task id="T01" parallel="true">
  <name>添加 ThemeContext provider</name>
  <read_files>
    src/theme/*
    src/lib/api-client.ts
    src/utils/storage.ts
  </read_files>
  <write_files>
    src/theme/ThemeContext.tsx
    src/theme/__tests__/ThemeContext.test.tsx
  </write_files>
  <action>
    导出 ThemeProvider 与 useTheme hook。
    主题值从 localStorage 读取，缺省读取系统 prefers-color-scheme。
    沿用 src/utils/storage.ts 的 `safeStorage` 包装（避免隐私模式报错）。
  </action>
  <verify>npm test -- theme/ThemeContext.test.tsx</verify>
  <done>测试通过；hook 在三种状态（light/dark/system）下返回正确值</done>
  <depends_on></depends_on>
</task>
```

## 输出

- `.specs/<change-id>/TASK.md`，包含所有任务的 XML 块 + 波次划分图

## 约束（强制）

- **R2.3**：每个任务必须有可执行的 `verify`，否则不允许进入 `DEV`
- 任务粒度太大（无法在 fresh context 完成）必须再拆
- 不允许「重构 X 模块」这种没有边界的任务

## 自检

- [ ] 每个任务都有完整的 7 字段（`id` / `name` / `read_files` / `write_files` / `action` / `verify` / `done`）
- [ ] **每个 `write_files` 都严格在 DESIGN 「触碰模块 + 新增模块」范围内**（B3 护栏）
- [ ] **任何任务的 `write_files` 都不包含 DESIGN 「禁动清单」中的文件**
- [ ] 每个任务的 `verify` 都是可执行命令
- [ ] 至少有 1 个 `[P]` 标记的并行任务（除非确实全是串行）
- [ ] 波次划分图清晰、无环依赖
- [ ] 任务编号连续
- [ ] **🛡️ Task 门禁已跑**：4 角色投票已执行，逐 task 自动化策略已写入 `<auto>` 字段，全票/多数通过后才触发下一步

## 🛡️ 专家团门禁 · Task 门（强制 · R9/R13）

TASK.md 生成后，**必须**启动该阶段的 4 领域专家审核，覆盖两个议题：
1. 任务拆分质量
2. **每个 task 的自动化策略**（自动执行 vs 人工介入）

### Task 门专家团构成（任务拆分阶段专有）

| 角色 | 核心关注点 |
|---|---|
| 🟫 **工程效能专家** | 任务粒度是否合理、波次划分是否最优、并行策略是否最大化效率 |
| 🟦 **架构师** | 任务是否覆盖 DESIGN 所有关键决策点、是否有遗漏的架构约束 |
| 🟩 **研发负责人** | 任务边界是否清晰、`write_files` 是否准确、工时预估是否合理 |
| 🔴 **资深测试工程师** | `verify` 是否可执行、`done` 是否可判定、覆盖率是否充分、各领域测试策略是否合理 |

### 议题 1 · 任务拆分质量

> 「TASK.md 拆分是否合理？粒度是否合适？依赖关系是否正确？边界是否清晰？」

### 议题 2 · 逐 Task 自动化策略（强制）

对 TASK.md 中的**每个 task**，专家团投票决定执行策略：

```
🗳️ 自动化策略: T<NN> <task名称>

   🟦 研发负责人: 🤖 自动化 / 👤 人工 <理由（关注：复杂度、风险、是否需要人工判断）>
   🟩 资深测试工程师: 🤖 自动化 / 👤 人工 <理由（关注：verify 是否可自动判定、边界是否清晰、各领域测试覆盖）>
   🔴 安全审计师: 🤖 自动化 / 👤 人工 <理由（关注：是否涉及敏感操作/权限变更）>

   结果: 🤖 自动化（全票/多数）→ 4-dev 自动执行，不暂停
         👤 人工（全票/多数）→ 4-dev 执行前暂停，等人工确认
         ⚠️ 平票 → 默认 👤 人工（安全优先）
```

**自动化判定标准**：
- 🤖 自动化：task 边界清晰、verify 可机判、无破坏性变更、无 schema 变更、无安全敏感操作
- 👤 人工：task 涉及破坏性变更（R4.6）、schema 变更（R4.5）、安全/权限改动、或 verify 需要人工判断

**逐 task 投票结果写入 TASK.md**，在每个 `<task>` 块中追加 `<auto>` 字段：

```xml
<task id="T01" parallel="true">
  <auto>true</auto>  <!-- 专家团投票：3/3 🤖 自动化 -->
  ...
</task>
```

### 投票格式（R13.3）

```
🗳️ Task 门: TASK.md 是否通过？

   🟫 工程效能专家: ✅/❌ <理由（关注：粒度、波次效率、并行策略）>
   🟦 架构师: ✅/❌ <理由（关注：覆盖设计决策点、遗漏的架构约束）>
   🟩 研发负责人: ✅/❌ <理由（关注：task 边界、write_files 准确性、工时）>
   🔴 资深测试工程师: ✅/❌ <理由（关注：verify 可执行性、done 可判定性、各领域测试策略）>

   自动化策略汇总：T01🤖 T02👤 T03🤖 ...
   结果: N/4 → <全票通过 ✅ 自动进入 4-dev / 多数通过 ✅ / 平票 ⚠️ 提交用户>
```

### 裁决规则（R13.2）

| 结果 | 动作 |
|---|---|
| 全票通过（4/4） | ✅ 自动进入 `4-dev.md`，按投票结果逐 task 执行 |
| 多数通过（3/4） | ✅ 自动进入，记录反对意见 |
| 平票（2/2）或争议 | ⚠️ 暂停，提交用户裁决 |
| 全票反对 | ⚠️ 暂停，重新拆分 |

### 通过后

进入 `4-dev`。**禁止**在全票/多数通过后还问「要继续吗？」（R13.4）。

## 触发下一步

门禁通过后：`@code-kit/prompts/4-dev.md`（按波次逐个执行，`<auto>true</auto>` 的任务自动执行，`<auto>false</auto>` 的任务执行前暂停等人工确认）