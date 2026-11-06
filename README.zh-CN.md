# ComposeSafe

[![CI](https://github.com/fengyunjx/ComposeSafe/actions/workflows/ci.yml/badge.svg)](https://github.com/fengyunjx/ComposeSafe/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/fengyunjx/ComposeSafe)](https://github.com/fengyunjx/ComposeSafe/releases)
[![License](https://img.shields.io/github/license/fengyunjx/ComposeSafe)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/fengyunjx/ComposeSafe)](go.mod)

[English](README.md)

**把 Postgres 镜像升了一个版本,容器起不来,而旧镜像又读不了新版本已经迁移过的数据——怎么办?**

ComposeSafe 让这件事不再可怕。它是 Docker Compose 栈升级的"保险箱":**升级前评估风险、做经过校验的快照,升级后验证,出问题就回滚**;还可以先在一份私有副本上完整演练一遍。
会改动东西的命令默认只做演练式预览(dry run)。命令是 `composesafe`,`csafe` 是别名。

<!-- TODO: 录好演示后取消注释并替换路径
<p align="center"><img src="docs/demo.gif" alt="ComposeSafe demo: failed upgrade rolls back automatically" width="720"></p>
-->

> 状态:**预发布(v0.x)**。除[限制](#限制)里列出的以外,下面的功能都已在真实 Docker 上测试过。

## 它能防什么

| 没有它 | 有了它 |
|---|---|
| 新镜像把数据库迁移到新格式后又失败,旧镜像再也读不了 | 从经过校验的快照把数据恢复到**新卷**;出故障的状态原样保留 |
| 浮动 tag(`latest`)被移动,"回退"又拉到新镜像 | 旧镜像在本地固定,用 `pull_policy: never` 重新启动 |
| Postgres 大版本升级需要导出/恢复,PG 18 还要改挂载点 | 自动在新卷里完成;你的原卷从不被写入 |
| 升级会不会出问题,只有上了生产才知道 | `rehearse` 在栈和数据的副本上跑真实升级,副本没有出网路径 |
| 备份做了却恢复不出来 | 校验和、归档完整性、导出结束标记,再加一次到临时服务器的试恢复 |

## 安装

一行安装(Linux / macOS,amd64 / arm64;会校验 SHA-256):

```bash
curl -fsSL https://raw.githubusercontent.com/fengyunjx/ComposeSafe/main/install.sh | sh
```

或从[发布页](https://github.com/fengyunjx/ComposeSafe/releases)下载对应平台的压缩包(`composesafe_<版本>_<系统>_<架构>.tar.gz`,附 `SHA256SUMS`),或自行构建:

```bash
make build        # bin/composesafe,以及 bin/csafe 链接
make install      # 安装到 $GOBIN
```

构建需要 Go 1.24+;运行需要**与项目同一台机器上的 Docker Engine 27+ 和 Docker Compose 2.24+**(远程 `DOCKER_HOST` 会被拒绝:备份要把宿主机路径挂进辅助容器)。

## 五分钟上手

```bash
cd 你的项目                  # compose.yaml 所在目录

composesafe scan                                     # 服务、镜像、卷,哪些服务有数据
composesafe plan db                                  # db 镜像的较新 tag
composesafe plan db=17.2                             # 评估:更新说明、风险、磁盘;写出 report.md
composesafe rehearse db=17.2                         # 在私有副本上演练整个升级,结束后清理
composesafe upgrade db=17.2                          # 预览:具体改哪一行、停哪些服务、要接受哪些风险
composesafe upgrade db=17.2 --execute                # 快照 → 修改 → 验证 → 失败自动回滚
composesafe rollback --execute                       # 手工回到最近的快照
composesafe history                                  # 快照、升级、演练记录
composesafe report latest                            # 某次运行的 Markdown 报告
composesafe cleanup old-volumes                      # 列出升级后遗留的旧卷(再加 --execute 才删)
```

想零风险试用:`composesafe scan -f testdata/stacks/toyapp/compose.yml -p toy`。

## 命令

| 命令 | 会改动吗 | 作用 |
|---|---|---|
| `scan` | 否 | 列出服务、镜像、卷,以及哪些服务有数据 |
| `plan <服务>[=tag]` | 否 | 候选 tag;指定目标时输出更新说明、风险规则、磁盘检查和 `report.md` |
| `snapshot <服务>...` | 加 `--execute` 才改 | 配置备份、镜像固定、`pg_dumpall`、卷冷归档、校验、试恢复;被停的服务一定会恢复运行 |
| `upgrade <服务>=<tag>` | 加 `--execute` 才改 | 评估、快照、**只改 image 的值**(注释和格式保留)、拉取、`up --no-deps`、健康检查 + 冒烟检查、失败自动回滚 |
| `rollback [--run id]` | 加 `--execute` 才改 | 还原文件和旧镜像;新版本跑过则把数据恢复到新卷 |
| `rehearse <服务>=<tag>` | 只动它自己的副本 | 在栈和数据的私有副本上跑真实升级;生产只被读取 |
| `history` / `report <id>` | 否 | 列出运行记录 / 输出报告(写 id 前缀或 `latest` 即可) |
| `cleanup <id\|old-volumes\|rehearsals>` | **会删除**(需 `--execute`) | 唯一会删除你数据的命令;每一项都要手输名称;不接受 `--yes` |

全局选项:`-f/--file`、`-p/--project-name`、`-c/--config`、`--execute`、`--yes`、`--json`、`--lang en|zh`、`--force-unlock`。
`--yes` 只跳过确认,不会绕过 BLOCK;HIGH/CRITICAL 风险需要 `--yes --allow-risk=<id>`(或交互时输入 yes)。

**退出码:** `0` 成功 · `1` 出错(含演练失败)· `2` 被风险规则阻断 · `3` 升级失败且已回滚 · `4` 升级失败**且**回滚也失败(会打印手工恢复指引并写入报告)。

## 它怎样保护数据

- 只通过 `docker` 命令行操作 Docker;除 `cleanup`(和演练自己的副本)之外不删除任何东西。CI 里有静态检查,别处出现破坏性调用会让构建失败。
- 快照必须**校验通过**才开始升级。快照失败也会把它停掉的服务重新启动。
- 回滚从不覆盖或删除:数据写进**新**卷/目录,并把你的 compose 指向它。Ctrl-C 不会自动回滚(状态不确定);`rollback --run <id>` 只靠快照目录就能接着做。
- 升级之后你若又改过 compose,`rollback` 会先显示逐行差异并询问。
- 机密:`.env` 的值和名字含 `PASSWORD/SECRET/TOKEN/KEY` 的变量会在日志和报告里被遮蔽。状态目录(`~/.local/state/composesafe/<项目>/<run-id>/`,权限 0600)里有导出和配置副本,请按机密对待。

### Postgres 大版本

`upgrade db=18-alpine` 不会原地升级数据。流程:停掉写入数据库的服务,做最终 `pg_dumpall`,在**新卷**(`<卷名>_pg18`,bind 目录则是 `<目录>_pg18`)上启动新版本的空集群,恢复导出,刷新统计信息(`ANALYZE`),对比表数量,再让写入方回来。PG 18 的挂载点会按新镜像要求改为 `/var/lib/postgresql`。迁移失败就直接切回未被动过的原卷。

### 演练

`rehearse` 用独立的项目名建一份副本(自己的容器和卷,不发布端口,**默认没有出网路径**,`--allow-egress` 才放开),复制数据(Postgres 对运行中的库做导出;其他卷按文件复制,源只读),先以当前版本启动,再在上面跑真实的 `upgrade`。冒烟检查在副本网络内的辅助容器里执行。无法隔离的栈(host 网络、Docker socket、可写的单文件挂载)会被拒绝。通过是证据而不是保证:数据量、负载和外部环境都会不同。

## 配置(`composesafe.yaml`,可选)

```yaml
version: 1
lang: zh                       # en | zh(也可用 --lang / COMPOSESAFE_LANG)
state_dir: ~/.local/state/composesafe
retention: {snapshots: 5}      # `history` 只提示,不会自动删除
services:
  db:
    snapshot: {mode: logical+volume, quiesce: [app, worker], restore_check: true}
  app:
    health_timeout: 120s
    smoke:
      - {name: http-ok, type: http, url: 'http://{{addr "app" 8080}}/healthz', expect_status: 200, retries: 10, interval: 3s}
      - {name: port-open, type: tcp, addr: '{{addr "app" 8080}}'}
```

冒烟类型:`http`、`tcp`、`exec`(容器内执行)、`command`(宿主机执行,不经 shell)。`{{addr "svc" port}}` 在生产解析为发布到宿主机的端口,在演练里解析为 `svc:port`。未知的键会被拒绝;配置里没有机密字段,API 密钥来自环境变量。环境变量:`COMPOSESAFE_STATE_DIR`、`COMPOSESAFE_LANG`、`GITHUB_TOKEN`(提高 GitHub 的访问频率上限)。

## 支持范围

| 数据 | 快照 | 升级 | 演练 |
|---|---|---|---|
| 官方 `postgres` 镜像,命名卷或 bind 目录 | 导出 + 冷归档 + 试恢复 | 同大版本:支持;大版本:导出/恢复到新卷 | 支持 |
| `postgis`、`timescaledb`、`bitnami/postgresql`、`pgvector` | 支持 | 仅同大版本;大版本升级会被**阻断** | 仅同大版本 |
| 其他命名卷 / bind 目录(MySQL、Redis、文件……) | 冷归档(服务会被停止) | 改镜像,数据回滚走归档 | 文件热拷贝(可能不一致) |
| 匿名卷、tmpfs、NFS/远程卷驱动 | 跳过并警告 | — | — |

## 限制

- **目前没有 LLM。** 更新说明只用确定性规则评估(关键词、版本比较、Postgres EOL 表)。`llm:` 配置会被接受但不起作用(会打印一条提示);LLM 摘要在计划中,而且永远不会成为必需项。
- 端到端测试在 Docker Desktop(macOS)上做过 Postgres 16 → 18;14 → 16、17 → 18 还没有用真实镜像跑过。CI 在 Linux 上跑集成测试。
- Postgres:自行设置 `PGDATA`、挂载 `/var/lib/postgresql/data` 的子目录、或以非 root 的 `user:` 运行的服务,没有做特殊处理。
- image 来自锚点、`extends`、`include` 或不在 `.env` 里的变量时,compose 无法自动修改:动手前就会告诉你原因。
- 在 Linux 上 Postgres 容器会接管 bind 目录的属主:大版本升级后的 `<目录>_pg18`(以及 bind 挂载数据库的演练副本)属于容器里的 postgres 用户,删除它需要 `sudo`。`cleanup` 从不删除 bind 目录。
- 演练对非 Postgres 数据是从运行中的卷拷贝,可能不一致;`command` 类冒烟检查在演练里会被跳过。
- 只支持本机 Docker 守护进程。

## 许可证

Apache License 2.0,见 [LICENSE](LICENSE)。

## 开发

```bash
make check         # vet、单元测试、gofmt、"绝不删除"检查
make lint          # staticcheck(go install honnef.co/go/tools/cmd/staticcheck@2025.1.1)
make integration   # 真实 Docker:20 个场景,约 15 分钟;CS_TEST_HELPER=alpine:3.21 可指定辅助镜像
make dist          # 在 dist/ 生成交叉编译的压缩包和 SHA256SUMS
```
