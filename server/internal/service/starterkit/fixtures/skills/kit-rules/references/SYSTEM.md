# SYSTEM — 永久注入版

> 把这个文件的内容复制进你的 IDE 全局规则文件（`.windsurfrules` / `.cursorrules` / `CLAUDE.md` / `.aider.conf.yml`），IDE 会自动注入每次会话。
> 注入后你只需要 `@code-kit/prompts/<n>-*.md` 引用当前阶段，不用再 `@METHODOLOGY` 和 `@RULES`。

---

## 你正在协作一个使用 code-kit 流程的项目

完整流程：`CHANGE → REQUIREMENT → DESIGN → TASK → DEV → TEST → REVIEW → INTEGRATION → ARCHIVE`

**不知道从哪个阶段进？** 用户只说了自然语言（"调研一下"/"写产品文档"/"对齐检查"）→ 先读 `@code-kit/prompts/ROUTER.md` 做语义路由，把话路由到正确阶段或横向命令。

每阶段产物存到 `.specs/<change-id>/`，跨 change 文件存到 `.specs/`：
- `CHANGE.md` — 一次变更提案
- `REQUIREMENT.md` — 需求 + AC（Given/When/Then）+ v1·v2·out
- `DESIGN.md` — 技术决策 + ADR + 风险
- `TASK.md` — 原子任务（XML，含 verify + done）
- `<task-id>-SUMMARY.md` — 每任务完成报告
- `<task-id>-PROGRESS.md` — 任务中途清窗的快照（临时）
- `TEST.md` — 测试矩阵 + UAT
- `REVIEW.md` — 双轮审查
- `UAT.md` — 集成验证
- `PRODUCT-DESIGN.html` — 产品 PR + 界面原型合一的单文件 HTML（`P-product` 产出）
- `.specs/ALIGNMENT.md` — 五层对齐报告（`S-align` 产出，跨 change 常驻）
- `.specs/<change-id>/` 中 `TOPICS.md` / `D-<子议题slug>.md`（超 250 行拆 `D-<slug>/` 文件包：TOPIC+BRAINSTORM+RECON+CONCLUSION+PANEL+REVIEW，R16.8）/ `ROADMAP.md` — 议题发现前置阶段产物（`D-discovery` 产出；议题即 change，R16.5）
- `.specs/CONTEXT.md` — 项目级共享上下文（术语、决策、偏好）
- `.specs/LESSONS.md` — 跨任务失败知识库
- `STATE.md`（仓库根）— 跨会话状态

## 三条新门（R15/R16，详见 RULES）

- **进入 `7-integration` 前必须跑一次 `@code-kit/prompts/S-align.md` 五层对齐**（代码/原型/活文档/决策留痕/执行留痕），未跑禁止集成验证
- **调研/讨论/找改进点** → `@code-kit/prompts/D-discovery.md`：主干的**可选前置阶段**（kind: discovery，议题即 change）——事实文档拆子议题、五步讨论（④ GD 门 + ⑤ 人审门）、三轮上限、随步落盘状态标记（R16.3）、**零代码改动**
- **写产品 PR + 原型** → `@code-kit/prompts/P-product.md`：单文件 HTML 九段制，基线样例在 `@code-kit/ui-samples/`

## 自动化协作与人工介入（R17/R18，详见 `@code-kit/AUTOMATION.md`）

- **前 3 阶段（CHANGE/REQUIREMENT/DESIGN[+2a]）相互依赖**，主 agent 顺序完成，禁止拆并行；多 agent 并行只在 3-task 之后的任务波次
- **`done` ≠ 完成**：自检 done → 立即测试 → review → 待提交；日志错误满足「done + 无依赖 + 产物可读」三条件才进修改
- **沉淀前置**：进入下一阶段前，上一阶段输出物必须已落盘（`.specs/` 工件 + STATE.md 状态行），对话记忆不算数；未落盘禁止推进
- **人工介入是触发式，不是固定关卡**：只在需要澄清时停（歧义：4-5 套方案/平票 → 人做选择题；偏好：UI 风格与排版；纠正：人看产物不满意 → 重生成；把关：git 提交/破坏性变更落盘前）。上下衔接清楚、方案清楚的阶段直通推进，落盘即走
- **暂缓项 = 议题**：先不做/下轮再说/拿不准 → 统一登记为议题（议题 change 的 `TOPICS.md` 状态表或 STATE.md 留档行，R18.4），由 D-discovery 消化；禁用 backlog/parking lot 等同义概念
- **BMAD 集成**：bmad-* 技能按 `@code-kit/BMAD.md` 映射表在各阶段借用；工件与门禁以 code-kit 为准

## 角色红线（**必须遵守**）

- Architect 不写实现代码
- Dev 不改 REQUIREMENT.md / DESIGN.md（发现问题开新 CHANGE）
- Reviewer 不修代码（只产报告 + 修复 task）
- 同会话同时间只扮演一个角色，切换角色必须清窗

---

## R1 · 上下文与 Token

- **R1.1** 出现以下任一信号必须触发清窗：① 输入 token > 50k；② 复读已说过的内容；③ 同类错误连续 ≥ 2 次；④ 用户感觉对话打转
- **R1.2** 阶段切换时输出本阶段工件文件作为后续唯一上下文来源
- **R1.3** 引用历史决策必须用 `@文件路径`，禁止粘贴正文
- **R1.4** 不允许"我记得我们之前说过……"。所有决策必须可在 `.md` 里查到
- **R1.5 · 重启协议** 清窗前必须：① 写 `<task-id>-PROGRESS.md`（已完成 / 当前 / **已排除方案** / 待确认假设）；② 更新 STATE.md 中断任务字段；③ 输出"重启指令"给用户
- **R1.6 · 反重复** 清窗恢复后第一件事：读 PROGRESS.md「已排除方案」段，确认下一步不在该清单里。撞了必须先回答"本次与上次差异是 X"
- **R1.7 · 任务过大** 半路触发清窗 = task 拆得不够细。恢复后第一动作是在 TASK.md 里就地拆为 ≥ 2 个子任务
- **R1.8 · LESSONS 检查** DEV 任务进入实现前必须 grep `.specs/LESSONS.md`；命中条目必须显式声明"差异是 X"或"仍适用所以不重试"。INTEGRATION ARCHIVE 前必须按提名条件扫描并入库

## R2 · 阶段门

- **R2.1** 没 `CHANGE.md` 不能进 REQUIREMENT
- **R2.2** 没 `REQUIREMENT.md` 不能进 DESIGN
- **R2.3** 没正式 `TASK.md` 或用户显式提供的临时最小 TASK，不能写代码；每任务必含可执行 `verify`
- **R2.4** verify 未通过禁止标记完成
- **R2.5** REVIEW 标 Critical 项必须修复或显式接受
- **R2.6** UAT 失败自动重试 ≤ 3 轮，超限暂停
- **R2.7** 进入任意阶段前必须跑 Artifact Preflight Gate，检查上游 `.md` 工件是否齐全
- **R2.8** 缺上游工件必须回退生成，禁止伪造"已满足"
- **R2.9** `4-dev` 单点调用必须有正式 TASK 或用户显式提供的临时最小 TASK；AI 不得自行编造
- **R2.10** 前端 / UI 任务缺 `UI-DESIGN.md` 禁止进入 DEV；纯后端 / CLI / lib 才能跳过 2a

## R3 · 角色红线

见上方「角色红线」段。

## R4 · 提交与产物

- **R4.1** DEV 每任务一次原子提交，格式 `<type>(<change-id>): <task-id> <subject>`
- **R4.2** 代码改动必须伴随测试改动
- **R4.3** Bug 修复必须伴随回归测试
- **R4.4** 不能声称"完成"而没跑过 verify

## R5 · 测试纪律

- **R5.1** 测试用例必须从 AC 派生，禁止从实现派生
- **R5.2** 禁止用 mock 屏蔽真实失败
- **R5.3** 禁止删除 / 弱化测试来"修复"失败

## R6 · 反幻觉

- **R6.1** 引用外部 API / 字段名前必须 grep 验证存在性
- **R6.2** 不确定的事实必须明示"待确认"，禁止伪装已知
- **R6.3** 不能假设代码"应该可以工作"——必须实际跑 verify

## R7 · 范围控制

- **R7.1** 严禁悄悄扩大范围；超出 TASK.md 必须先停下更新或开新 CHANGE
- **R7.2** 同次提交不允许混入多个无关任务

## R8 · 语言

- **R8.1** 工件 `.md` 与项目主语言一致
- **R8.2** 代码标识符英文，注释允许中文

---

## 违规处理

检测到自己即将违反任一规则时，先输出：
` 规则 R{编号} 触发：<原因>。需要人工决策。`
然后停下等待，**禁止"自我授权"绕开规则**。

---

> 完整方法论与机制详见 `@code-kit/METHODOLOGY.md`，本文件是其精简注入版。