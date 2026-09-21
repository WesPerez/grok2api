# 本机 Grok 4.7 部署

生产链路为 Router → Sub2API 账号 2221 → Nginx `/grok2api/` → grok2api → Resin
`AppsGlobal.{account}`。Sub2API 到本机服务使用直连代理；账号出口身份在 grok2api 内部生成。

源码同步 chenyme/grok2api main `5e5ad755`（v3.1.6 后的 Build 1.0.40 适配）。
保留 `/grok2api` 前端路径和独立 Docker 网络，以及 Grok 4.7 的四档推理、500k 上下文、
图片能力及官方价卡。推理菜单优先使用 Build 模型目录，静态能力作回退。模型从 Build `/models` 发现，
不把 4.7 自动伪装成 4.6，也不为旧版本猜测兼容路由。

## 发布与回滚

推送 `main` 后由 `GHCR Image` 工作流执行后端测试、vet、Swagger 校验、前端 test/lint/build，
再发布 `ghcr.io/wesperez/grok2api:sha-<40位提交>`。部署必须核对镜像 revision 并锁定 digest。
同一工作流允许在 `main` 手动触发完整验证与发布，便于恢复未触发的 fork 工作流；其它分支手动触发只构建验证。
本机不构建镜像。Compose 必须从 `/root/grok2api` 读取原有 `.env` 和 `config.yaml`，
保留 `GROK2API_PORT=127.0.0.1:18000`，不得在临时源码工作树直接启动默认 Compose。

升级前用 SQLite backup API 保存一致的数据库副本，并将配置和旧镜像 digest 放入权限 0700
的专用恢复目录。回滚必须把数据库、加密配置和旧镜像作为同一组恢复；不要让旧程序打开
已升级的数据库。凭据、数据库和完整上游响应禁止入库。

## 出口契约

容器地址为 `172.30.0.2`，节点 1 的代理模板是
`socks5h://AppsGlobal.{account}:<secret>@172.17.0.1:10834`。不同账号生成不同身份，
同一账号复用稳定 lease；不保证每个账号独占一个公网 IP。

10834 是 Resin 的 NAT 虚拟入口，当前蓝绿目标为 10835/10836。UFW 在 DNAT 后检查目标端口，
所以必须仅放行 grok2api 网桥上来自 `172.30.0.2`、到 `172.17.0.1:10835:10836` 的 TCP 流量。
2026-09-22 的刷新超时源于缺少此规则。网桥重建后要核对接口名并同步规则；不得向公网开放
代理端口。验收包括容器内 TCP 连通、管理员出口测试、凭据刷新和不同账号的 Resin lease。

## 质量保护

使用上游原生 `qualityGuard.requestRetry`，显式启用、`maxAttempts: 6`、
`onExhausted: fail_closed`。它在响应提交前检测缺失推理/空流并排除已试账号，保留上游对
账号绑定状态及有副作用工具的重放限制。耗尽后返回明确错误，不交付异常响应。

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

官方能力和计价来源：https://docs.x.ai/developers/models/grok-4.7 。
