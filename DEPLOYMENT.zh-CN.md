# 本机 Grok 4.7 部署

生产链路为 Router → Sub2API 账号 2221 → Nginx `/grok2api/` → grok2api → Resin
`AppsGlobal.{account}`。Sub2API 到本机服务使用直连代理；账号出口身份在 grok2api 内部生成。

源码基线为 chenyme/grok2api main `8535932b0783db2033657f36e2e1a84196c4e151`（v3.1.8）。
`main` 保持纯上游，`mine` 保留下述定制并作为默认分支。
保留 `/grok2api` 前端路径和独立 Docker 网络，以及 Grok 4.7 的四档推理、500k 上下文、
图片能力及官方价卡。推理菜单优先使用 Build 模型目录，静态能力作回退。模型从 Build `/models` 发现，
不把 4.7 自动伪装成 4.6，也不为旧版本猜测兼容路由。

## 发布与回滚

推送 `mine` 后由独立 `MINE Image`（`mine-image.yml`）工作流执行后端测试、vet、Swagger 校验、前端 test/lint/build，
再发布 `ghcr.io/wesperez/grok2api:sha-<40位提交>`。部署必须核对镜像 revision 并锁定 digest。
同一工作流允许在 `mine` 手动触发完整验证与发布，便于恢复未触发的 fork 工作流；其它分支手动触发只构建验证。
本机禁止安装/下载项目依赖、编译、构建及打包；测试与构建仅运行在 GitHub-hosted CI，详见 [AGENTS.md](AGENTS.md)。Compose 必须从 `/root/grok2api` 读取原有 `.env` 和 `config.yaml`，
保留 `GROK2API_PORT=127.0.0.1:18000`，不得在临时源码工作树直接启动默认 Compose。

本机手工发布使用 `/opt/server-scheduled-tasks/runtimes/storage-maintenance/current/release-retention/compose-release.sh grok2api`
调用既有 Compose 的 `pull` / `up -d --no-build --pull never`，与宿主回收器共用
`/run/lock/grok2api-deploy.lock`。宿主 [release-retention](https://github.com/WesPerez/server-scheduled-tasks/tree/main/tasks/release-retention)
每 15 分钟保护实际运行容器的 image ID，并在发布/健康观察 3 分钟后回收旧镜像。
本机仅保在用版本，不维持 previous、旧 pin 或镜像回滚副本；源码、数据和凭据恢复链单独保留。

正式 wrapper 固定读取 `docker-compose.yml`，不会自动合并 override。本文件已将生产
`grok2api_internal` 网络、`172.30.0.2` 固定地址及 `json-file` 的 `20m × 3` 轮转配置
归入主 Compose；旧 `docker-compose.override.yml` 已退役。发布前在同步后的规范目录，
通过下列正式入口的 `config --format json` 核对解析配置，仅显示服务的网络、端口、卷和日志字段。
确认镜像为本次验收的 digest，端口、数据挂载、网络/IPAM、地址与日志限额均匹配现有容器。

```bash
/opt/server-scheduled-tasks/runtimes/storage-maintenance/current/release-retention/compose-release.sh grok2api config --format json | python3 -c 'import json,sys; d=json.load(sys.stdin); s=d["services"]["grok2api"]; print(json.dumps({"service": {k:s.get(k) for k in ("image","networks","ports","volumes","logging")}, "networks":d.get("networks")}, indent=2))'
/opt/server-scheduled-tasks/runtimes/storage-maintenance/current/release-retention/compose-release.sh grok2api pull grok2api
/opt/server-scheduled-tasks/runtimes/storage-maintenance/current/release-retention/compose-release.sh grok2api up -d --pull never --wait --wait-timeout 180 grok2api
```

wrapper 为每次调用取得登记发布锁，并强制 `up --no-build`。发布命令指定 `grok2api`，
不启用 `quality-guard` profile；可选 egress sidecar 的 build 定义只可在远程构建环境使用。
本机若要新增该 sidecar，须另行验证 hosted 镜像并登记部署合同，不能执行上游 build 示例。

2026-11-22 候选使用合成环境及 `--env-file /dev/null` 做静态 Compose 解析，网络、固定地址、
日志限额、端口和卷名与实际运行容器一致；默认服务列表只有 `grok2api`。该解析不启动服务，
不替代修改后最终 SHA 的 hosted CI 与正式发布验收。

升级前用 SQLite backup API 保存一致的数据库副本，并将配置、原源码 SHA 和镜像 digest 身份
记录放入权限 0700 的恢复目录。恢复需核对数据库、加密配置和程序版本兼容性；必要镜像从
已验证远端摘要取得，不在本机额外保旧镜像。不要让旧程序打开已升级数据库。凭据、数据库
和完整上游响应禁止入库。

## 出口契约

容器地址为 `172.30.0.2`，节点 1 的代理模板是
`socks5h://AppsGlobal.{account}:<secret>@172.17.0.1:10834`。不同账号生成不同身份，
同一账号复用稳定 lease；不保证每个账号独占一个公网 IP。

10834 是 Resin 的 NAT 虚拟入口，当前蓝绿目标为 10835/10836。UFW 在 DNAT 后检查目标端口，
所以必须仅放行 grok2api 网桥上来自 `172.30.0.2`、到 `172.17.0.1:10835:10836` 的 TCP 流量。
2026-09-22 的刷新超时源于缺少此规则。网桥重建后要核对接口名并同步规则；不得向公网开放
代理端口。验收包括容器内 TCP 连通、管理员出口测试、凭据刷新和不同账号的 Resin lease。

## 质量保护

使用 `qualityGuard.requestRetry`，显式启用、`maxAttempts: 6`、`onExhausted: fail_closed`。
2026-09-22 修正了将快速合包、高推理 Token 比例和密文长度比例当作质量异常的规则。
相同 SSE 无论分包还是合包，推理摘要、密文和工具调用都按实际事件判定；工具调用不要求
额外输出可见思考。只检查已完成的无推理纯文本，30 秒观察期限后放行已有正文；空流和
明确上游错误独立处理，不混成 `missing_thinking`。保留账号绑定状态及有副作用工具的重放限制。
只有协议终止事件才表示响应完成；正文后的意外 EOF 由传输层记录为流不完整，
不会触发缺推理重试或 12 小时账号冷却。
工具结果后的答复允许直接给出正文，不要求再输出一段思考；只豁免紧接工具结果的这一轮，
后续用户新请求仍执行原检查。空流、上游错误、账号绑定和工具重放限制继续生效。

本机 `providerBuild.streamIdleTimeout` 通过管理员运行设置从 `2m` 调整为 `5m`，
支持 xhigh 的长推理间隔，并保持有界的语义空闲检测。设置写入数据库并热加载，
不能只修改启动 YAML 后假定生效。

旧的外置 `grok-quality-retry-proxy`、Sub2 `x-sub2-quality-retry` header override、
Nginx 动态端口映射及本地 protected-retry middleware 均退役。Nginx 直接转发 18000。
原先退出的 sidecar 不再承担请求质量保护，`qualityGuard.enabled: false`；
`requestRetry.enabled: true` 独立生效。上游可选 sidecar 工具保留在源码中。

## 验收

- 确认 `/healthz`、`/readyz`、前端子路径及镜像 revision。
- 刷新账号凭据、额度和模型目录，分别记录有效、停用、失效凭据和额度不足数量。
- 管理端「检测账号」固定检测 `grok-4.7`，模型级拒绝/额度阻断也按 `grok-4.7` 保存。
  Build 余额快照是按需更新的，验证额度前显式调用目标账号 `refresh-billing`；已有快照
  不会因凭据或模型同步自动变新。检测请求默认推理档位与单独的 xhigh 链路验收分开记录。
- Sub2 账号 2221 只映射完整名称 `grok-4.7`；通过管理员 API 更新以刷新调度缓存。
- 逐层验证 Responses 普通/流式、工具往返、图片输入和真实模型归属；HTTP 200 中的 SSE
  `response.failed` 不算成功。Router 验收必须核对实际模型，不能把降级到其它模型当作 Grok 成功。

`tools/smoke-grok-responses.py` 发送两轮无副作用的 Responses 请求，验证 Windows 路径、
Unicode、反斜杠和换行的工具参数保真，以及模型是否消费工具返回值。它不会执行模型工具，
不会自动重发，发现错误终态或实际模型不一致即失败。该工具属于原始 HTTP 协议验收；
Codex 客户端验收需要另用真实客户端，并核对 Router 审计中的实际路由。
工具结果消费、模型是否精确抄写输入字符串、流式参数与最终参数是否一致分别报告；
例如模型把 CRLF 生成成 LF 时仍继续验证第二轮，但总结果 `passed` 保持失败。

官方能力和计价来源：https://docs.x.ai/developers/models/grok-4.7 。

## 个性化提交维护

当前上游基线 `8535932b0783db2033657f36e2e1a84196c4e151`。个性化归并为部署与子路径、账号与模型能力、网关流式质量恢复三项职责。旧主线 `62dc5592` 和测试分支 `e18ce68f` 的有效改动映射见下表；现存 refs 中没有原文所称 `archive/pre-governance-20260922`，因此清理前必须另行建立并核对真实源码恢复引用。

普通日志优先使用应用原生保留策略；容器输出最多 20 MiB × 3。凭据保存在仓库外的 root 私有目录，禁止提交到公开 fork。


### 2026-11-22 上游整合与最终验收

| 旧提交 | 当前职责与处理 |
| --- | --- |
| `e7b12aed`, `62dc5592` | 部署、前端子路径和备用 CI 原来源身份；独立 `mine-image.yml` 替代定制旧 workflow |
| `95a9affb`, `b80348fa`, `e89afeb8`, `ea26be15`, `e18ce68f` | 离线空账号启动、管理鉴权、诚实模型目录、前端资源与 ingress 重写验收；绑定本次候选 digest |
| `1e06fccf` | Grok 4.7 模型能力及账号检测 |
| `86310292` | 流式协议完成、纯文本缺推理恢复、工具结果回合豁免、传输分包一致性及工具参数验收 |

上游新增 `requestRetry.autoDisable` 与独立输出吞吐惩罚保持可配置且默认关闭。
开启 `autoDisable` 会同时开启请求质量检测；首次明确缺推理或达到所配置输出吞吐阈值会停用
账号。这个显式开关与纯文本缺推理分类分别处理；后者不按到达速度、Token 或密文比例判缺推理。
上游 Console 未绑定账号自动删除也保持默认关闭，不改变现有运行配置。

远程 CI 完成全套前后端检查及 amd64/arm64 构建后，只生成不可变 `sha-<完整 SHA>` 候选。
合并后的镜像 digest 必须在 GitHub-hosted runner 上通过离线空账号实例验收，才更新 `mine`、
`latest` 别名并上传接受的 image metadata。该验收使用真实候选镜像，验证原生管理鉴权、
无可用模型时 503 readiness、存活健康和带前缀资源；它不替代真实推理或浏览器渲染验收。

分支切换顺序：先保全旧源码 → 推送并索引独立 `mine-image.yml` → 默认分支设为 `mine`
→ 在仓库层停用上游 `ghcr-image.yml` → 精确 lease 同步纯上游 `main`。随后复用
`gci build mine-image.yml --ref mine` 的原自动请求，核对最终 SHA 和完整成功 job 集。

最终远端分支发布后，确认 `/root/grok2api` 干净且无其他写入者，fetch 并同步到同一
`origin/mine` SHA；核对本地 HEAD、远端 SHA、纯上游 main 和状态。运行 revision/digest
另行经正式发布入口验证；从临时工作树推送后拉镜像不会自动更新规范源码目录。
