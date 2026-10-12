---
name: grilling
description: "交互引导与思维工具：质询、访谈、头脑风暴、多角色圆桌、深度追问、教学、向导脚本、BMAD 定制与深化。"
---
Interview the user relentlessly until you reach a shared understanding. Map this as a **design tree**: every decision branches into the decisions that hang off it.

Work the tree in **rounds**. The **frontier** is every decision whose prerequisites are already settled: the questions you can ask _now_ without guessing at answers you haven't heard yet. Ask the whole frontier in one round: number each question and give your recommended answer. Then wait for the user's answers before the next round.

Format a round like so:

```
❓ **Q1** - **<question title>**: <question body, might be multiple paragraphs, including multiple choices>

➡️ <your recommended answer>

---

❓ **Q2** - **<question title>**: <question body, might be multiple paragraphs, including multiple choices>

➡️ <your recommended answer>
```

Each round the user answers reshapes the tree: settled decisions push the frontier outward and unblock questions that depended on them. Recompute the frontier and ask the next round. A question whose answer depends on another question still open in this round belongs to a _later_ round, not this one.

Finding _facts_ is your job, never the user's. When a frontier question needs a fact from the environment (filesystem, tools, etc.), dispatch a sub-agent to find it; don't ask the user for anything you could look up yourself. Don't block on it: a running exploration is an unsettled prerequisite, so only the questions downstream of it wait for the sub-agent to report; ask the rest of the frontier now. The _decisions_ are the user's: put each to them and wait.

The session is done when the frontier is empty: every branch of the design tree visited, nothing left silently assumed. Do not act on it until the user confirms you have reached a shared understanding.

---

## 打包内容（references/）

本技能把同主题的多个方法论文档打成一个文件包：`SKILL.md` 只保留主流程与索引，
其余内容按需读取 `references/<名称>.md`，不要一次全部载入。

| 参考文件 | 说明 |
|---|---|
| `references/interview-me.md` | 一次只问一个问题，直到对真实意图有约 95% 把握。 |
| `references/loop-me.md` | 就你想搭建的工作流规格质询你，限定在本工作区内。 |
| `references/wait-what.md` | 上一段话没听懂时使用：请求用简化技术英语与项目统一语言重新讲一遍。 |
| `references/teach.md` | 在本工作区内教用户一项新技能或新概念。 |
| `references/wizard.md` | 生成交互式 bash 向导，带人类走完只有人类能做的步骤（开通账号、配置密钥、一次性迁移）。 |
| `references/idea-refine.md` | 把粗糙想法通过结构化发散与收敛打磨成锐利、可执行的概念。 |
| `references/bmad-brainstorming.md` | 用多样化创意技法主持一场头脑风暴。 |
| `references/bmad-forge-idea.md` | 用多角色质询打磨一个半成品想法，直到你能放心行动或果断放弃。 |
| `references/bmad-party-mode.md` | 主持多角色圆桌讨论，或编写自定义角色阵容。 |
| `references/bmad-advanced-elicitation.md` | 推动模型重新审视并改进上一次输出，含苏格拉底式、第一性原理、事前验尸、红队等技法。 |
| `references/bmad-customize.md` | 编写与更新已安装 BMad 技能的定制覆盖，改变其行为。 |

主流程来自 `grilling`；下面各项是打包进来的独立方法论。
