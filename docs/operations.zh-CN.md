# ERC-4337 v0.7 Bundler 部署说明

[中文首页](../README.zh-CN.md) · [English deployment guide](operations.md)

## 配置一套服务

从[示例配置](../config.example.json)复制私有配置文件。每套服务对应一条支持 EIP-1559 的链、一个 v0.7 EntryPoint、一名有余额的外层签名账户和一个费用受益地址。运营方核对合约运行时代码的 Keccak-256，将 `startBlock` 设为需要追踪的最早区块或更早高度。同一数据库的所有副本须使用相同的链、EntryPoint、发送方、受益地址和起始高度。

| 配置项 | 运维需要提供 |
|---|---|
| `dbDriver`、`dbEnv` | 多机部署使用 `pgx`，环境变量保存 PostgreSQL 主库或高可用主库入口。数据库主备切换和备份由部署环境提供。本机测试可使用 `sqlite` 和本地文件 DSN，附加 `?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate`。 |
| `executionEnv`、`backupExecutionEnvs` | 主、备用读 RPC 对应的环境变量名。每个端点应支持按区块哈希读取、交易回执、日志、费率估算和兼容 Geth 的 `debug_traceCall`。内置模拟器还使用状态覆盖。读取失败时转至另一家已验证的提供方。 |
| `simulationArtifact`、`approvedValidationCode` | 内置的运营方管理池使用下方指定的 v0.7 模拟制品。录入已审查的账户代理、实现合约、工厂、代付合约等验证路径代码哈希。准入时核实升级权限、存储访问和适用的 ERC-7562 规则，适用于受控的钱包及代付体系。 |
| `validationEnv` | 可选的私有验证服务。配置后，估算和绑定区块的验证采用下文的适配接口，可接入团队的完整公共池策略。 |
| `signerEnv` | 私有签名端点，通过 `eth_signTransaction` 签署 Type 2 交易。在签名策略中限定链、EntryPoint、受益人和费用预算。Bundler 逐次验证签名者和原始待签内容。 |
| `broadcasterEnv`、`broadcastTokenEnv` | GM Wallet Broadcaster 的服务地址和业务令牌。先在目标链为 Broadcaster 准入节点，再为各副本配置同一提交方身份。 |
| `webhookSecretEnv`、`clientTokenEnvs` | Broadcaster 回调设为 `https://<bundler>/webhooks/broadcaster`，使用同一 HMAC 密钥。每个业务令牌和回调密钥至少 24 字节。钱包入口由 TLS 网关承载，签名和验证端点仅对内部开放。 |
| `poolSize`、`maxOps`、`maxBytes`、`maxGas`、`maxFeeWei`、`pollMillis` | 分别限制操作池、包内操作数、编码后 calldata、外层 Gas、签名账户费率支出及调度间隔。示例每包最多 16 笔。节点短暂断连后继续核查原任务。 |
| `confirmations` | 含入块本身，达到配置深度后标记为 `FINAL`。观察到规范链变化时仍会回查。结算深度由业务按目标链配置。 |

内置模式从指定 npm 版本取模拟制品，程序启动时再次验证文件摘要。[官方 v0.7 合约包](https://www.npmjs.com/package/@account-abstraction/contracts/v/0.7.0)

```sh
curl -fsSL 'https://registry.npmjs.org/@account-abstraction/contracts/-/contracts-0.7.0.tgz' |
  tar -xzO package/artifacts/EntryPointSimulations.json > EntryPointSimulations.json
go build -o bin/bundler ./cmd/bundler
./bin/bundler -config config.json
```

## 生命周期与恢复

| 可见状态 | 进入条件与后续处理 |
|---|---|
| `QUEUED` | 在记录的链头通过模拟并持久化。同 Nonce 的主动替换须提供有效签名，且两项费率上限均至少增加 10%。 |
| `BUNDLED`，外层 `SIGNED` | 工作者复核候选、模拟整包、核对外层签名，将原文和成员清单原子提交。任何接管副本都能用同一原文继续发送。 |
| 外层 `SUBMITTED` | Broadcaster 已持久化任务，可从 `broadcast.attempts` 查看外层候选。响应超时按原哈希查询。收到加价请求后，在预算内生成同 Nonce 的签名替换，先落库再提交。 |
| `INCLUDED` | 在规范块回执中找到匹配的 EntryPoint 事件，逐笔返回执行成功或回退。 |
| `FINAL` | 达到配置深度。回执查询核对规范块，重组后撤销孤块回执并重新验证受影响操作。 |
| `REPLACED`、`CONSUMED`、`EXPIRED`、`REJECTED` | 本地被替换、其他操作在链上占用该槽、有效期届满，或二次验证明确失败。状态接口附具体原因。 |

通过 `gm_getUserOperationStatus([userOpHash])` 查询本服务已受理操作的内部进度。钱包标准方法使用 ERC-7769 响应格式。回调使用原始请求体核验 HMAC-SHA256，仅唤醒持久化任务。回调中断时，轮询和 EntryPoint 索引继续推进。多个副本分别竞争组包、广播和索引租约，租期使用数据库时钟。网络请求允许重复，只有持有当前租约的实例能提交状态变化。

## 同事接入点

| Go interface | 实现约定 |
|---|---|
| `Signer` | 在 `Sign(ctx, unsignedTx, from)` 接入公司 KMS/HSM，只返回签名交易。已有 RPC 适配器支持十六进制原文或带 `raw` 的对象。 |
| `Validator` | 实现 `Estimate`、`Validate`。远程适配器调用 `eth_estimateUserOperationGas(op, entryPoint)` 和私有 `gm_validateUserOperation(op, entryPoint, blockHash)`。返回 `blockHash`、十六进制 `prefund`、代付时的 `paymasterDeposit`，以及整数 Unix 时间戳 `validAfter`、`validUntil`（0 表示没有该边界）。验证签名、有效期、质押、存储和指令规则；确定性拒绝返回结构化 RPC 错误。 |
| `Broadcaster` | 已实现的 HTTP 客户端通过 `/EVM` 获取建议 Nonce、查哈希、提交和显式替换。各副本使用同一业务身份。 |

## 运行与验证

`GET /ready` 在检查点到达规范链头或其父块后就绪。鉴权的 `GET /metrics` 提供队列深度、最长等待时间、索引高度、工作错误计数、受理和回执查询延迟直方图。部署侧同时观测签名地址余额、数据库、节点延迟和 Gas 收支。回执查询使用独立于模拟的并发容量。

```sh
go test -race -shuffle=on ./...
go vet ./...
```

可选端到端测试自行启动本地 Anvil 和 Broadcaster。准备 Foundry Anvil（已测 1.7.1）、Broadcaster 可执行文件（已测提交 `a499e08`）及官方 v0.7 npm 包的 `package/artifacts` 目录。设置 `TEST_PG_DSN` 可在临时 PostgreSQL 数据库中验证接管，测试创建并清理自己的 schema。默认使用 SQLite。设置 `TEST_PROFILE=1` 可运行 48 笔、并发 6 的本地突发测量，输出受理和回执分位延迟。

```sh
TEST_ANVIL=/path/to/anvil TEST_BROADCASTER=/path/to/broadcaster \
  TEST_AA_ARTIFACTS=/path/to/package/artifacts \
  go test -count=1 -v -run TestAnvilBroadcaster ./bundler
```

生产发布时使用实际签名服务和节点群验证稳定流量、突发流量、读节点下线和数据库切换。开放公共池前按所选策略运行对应版本的兼容性套件。[v0.7 上游测试](https://github.com/eth-infinitism/bundler-spec-tests/tree/releases/v0.7)
