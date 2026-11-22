<p align="center"><img src="https://github.com/GMWalletApp.png?size=96" width="80" height="80" alt="GM Wallet"></p>
<h1 align="center">GM Wallet ERC-4337 Bundler</h1>
<p align="center"><strong>提交智能账户签好的操作，用同一个哈希从受理跟踪到链上执行结果。</strong></p>
<p align="center"><a href="README.md">English</a> · <strong>简体中文</strong> · <a href="LICENSE">GM Wallet Community License 2.2</a></p>
<p align="center"><a href="#钱包接入">钱包接入</a> · <a href="#操作生命周期">操作生命周期</a> · <a href="#生产部署与验收">生产部署与验收</a></p>

本项目提供可运行的 Go Bundler，支持 EntryPoint **v0.7** 和运营方管理的操作池。钱包用 `userOpHash` 查询单笔操作。运营方可以用 PostgreSQL 运行多个实例，并通过 GM Wallet Broadcaster 投递已签外层交易。

## 快速启动

需要 Go 1.26.2 或更高版本、支持追踪的执行节点、已配置的签名服务和 Broadcaster。复制示例后，按部署说明填写合约代码哈希、受益地址、索引起点及凭证环境变量，并安装 v0.7 模拟合约文件。

```sh
cp config.example.json config.json
go build -o bin/bundler ./cmd/bundler
./bin/bundler -config config.json
```

启动后用 `GET /ready` 检查索引是否追上链头。[配置、状态表、同事接入接口和端到端验证](docs/operations.zh-CN.md)

## 一笔操作如何执行

钱包把智能账户要执行的操作构造成 `UserOperation` 并签名。Bundler 验证并接收操作，把相容的操作打包成一笔调用 EntryPoint 的交易。运营方的签名账户先垫付外层 Gas。EntryPoint 按每笔操作向账户或 Paymaster 收费，并向配置的受益地址结算。钱包用自己的 `userOpHash` 跟踪结果。[ERC-4337](https://eips.ethereum.org/EIPS/eip-4337)

| 参与方 | 职责 |
|---|---|
| 钱包或商户服务 | 根据账户合约构造 calldata、估算 Gas，按需获取代付授权，签署最终操作并保存订单与操作哈希的关联。 |
| Paymaster | 决定是否赞助，提供所需的数据和签名，维持在 EntryPoint 的存款。 |
| Bundler | 按链和 EntryPoint 验证操作、维护等待池、组包、请求外层签名，逐笔索引链上执行结果。 |
| GM Wallet Broadcaster | 通过 `POST /EVM/transactions` 接收**已签名的外层交易**，按发送账户的 Nonce 调度，向可用 RPC 并发广播，跟踪替换版本并回调 Bundler。[Broadcaster 接口](https://github.com/GMWalletApp/Broadcaster/blob/main/docs/api.zh-CN.md) |

**兼容范围。** 当前实现使用 EntryPoint **v0.7**，与 GM Wallet Broadcaster 的以太坊配置示例对齐。每个部署固定 chain ID、EntryPoint 地址、运行时代码哈希、ABI 和操作哈希算法，通过验证后才向钱包公布。v0.8/v0.9、EIP-7702 授权、聚合签名和公共操作池对等网络分别验收后启用。Broadcaster 已接入的 EVM 链仍需验证 EntryPoint 和 RPC 模拟能力才能开通 Bundler。[GM Wallet 配置](https://github.com/GMWalletApp/Broadcaster/blob/main/config.example.json) · [EntryPoint 版本记录](https://github.com/eth-infinitism/account-abstraction/releases)

## 钱包接入

以下示例使用 curl 和 jq。服务部署后，把 `BUNDLER_RPC_URL` 设为目标链的 HTTPS 地址，`BUNDLER_API_TOKEN` 设为接入凭证。钱包通过 HTTP POST 调用 JSON-RPC 2.0。[ERC-7769](https://eips.ethereum.org/EIPS/eip-7769)

| 方法 | 参数 | 钱包如何使用 |
|---|---|---|
| `eth_chainId` | `[]` | 签名前核对链 ID。 |
| `eth_supportedEntryPoints` | `[]` | 选择账户合约和当前服务都支持的 EntryPoint。 |
| `eth_estimateUserOperationGas` | 操作草稿、EntryPoint | 填入符合账户要求的占位签名，取得各部分的 Gas 估算。 |
| `eth_sendUserOperation` | 已签操作、EntryPoint | 保存返回的哈希。它表示操作已通过验证并持久化入池。 |
| `eth_getUserOperationByHash` | 操作哈希 | 查询操作原文和可用的入块位置。仍在等待时可能返回 null。 |
| `eth_getUserOperationReceipt` | 操作哈希 | null 表示暂时没有入块回执。取得回执后读取当前操作的 `success`、费用、日志和外层交易回执。 |
| `gm_getUserOperationStatus` | 操作哈希 | 查看本服务保存的调度状态、Broadcaster 任务 ID 和外层交易候选。 |

### 示例：提交一笔操作

先查询网络和可用 EntryPoint，选择与账户合约版本匹配的地址，存入 `ENTRY_POINT`。

```sh
rpc() {
  curl -fsS "$BUNDLER_RPC_URL" -H "Authorization: Bearer $BUNDLER_API_TOKEN" \
    -H 'Content-Type: application/json' --data-binary @-
}
jq -n '{jsonrpc:"2.0",id:1,method:"eth_chainId",params:[]}' | rpc
jq -n '{jsonrpc:"2.0",id:2,method:"eth_supportedEntryPoints",params:[]}' | rpc
```

准备 `operation.json`，填入 v0.7 RPC 操作对象。Nonce、Gas 和费率使用十六进制数值，calldata 和签名使用十六进制字节串。部署新账户时同时提供 `factory`、`factoryData`。使用代付时提供 `paymaster`、`paymasterData`、`paymasterVerificationGasLimit` 和 `paymasterPostOpGasLimit`。签名方通过 `EntryPoint.getNonce(sender, key)` 查询账户在对应通道的操作 Nonce。

下面是已部署、自付 Gas 账户的 RPC 结构示例。估算前换成实际发送地址、Nonce、调用数据、费率和账户验证器接受的占位签名。

```json
{
  "sender": "0x1111111111111111111111111111111111111111",
  "nonce": "0x0",
  "callData": "0x",
  "callGasLimit": "0x186a0",
  "verificationGasLimit": "0x30d40",
  "preVerificationGas": "0xc350",
  "maxFeePerGas": "0x77359400",
  "maxPriorityFeePerGas": "0x3b9aca00",
  "signature": "0x"
}
```

```sh
op_request() {
  jq -n --arg method "$1" --arg ep "$ENTRY_POINT" --slurpfile op "$2" \
    '{jsonrpc:"2.0",id:3,method:$method,params:[$op[0],$ep]}'
}
op_request eth_estimateUserOperationGas operation.json | rpc
```

填入估算结果和费率上限，完成代付授权，按账户合约规则签名，将最终 RPC 对象保存为 `signed-operation.json`。提交响应丢失时重交原始已签操作。

```sh
USER_OP_HASH=$(op_request eth_sendUserOperation signed-operation.json | rpc |
  jq -er 'if .error then error(.error|tostring) else .result end')
jq -n --arg hash "$USER_OP_HASH" \
  '{jsonrpc:"2.0",id:4,method:"eth_getUserOperationReceipt",params:[$hash]}' | rpc
```

商户应在满足该链确认策略后，根据**自己这笔操作**的回执结算。同一笔成功的外层交易中，一笔操作可能成功，另一笔可能回退。费用对账读取 `actualGasCost`，执行回退时保存可获得的原始原因。

### 受理错误

JSON-RPC 返回结构化错误，并保留可获得的 EntryPoint `AAxx` 原因。字段无效或 EntryPoint 不受支持返回 `-32602`，EntryPoint 验证失败返回 `-32500`，代付验证失败返回 `-32501`，违反验证规则返回 `-32502`，有效时间窗口不合适返回 `-32503`，签名失败返回 `-32507`，代付存款无法覆盖在途操作返回 `-32508`。接入方可区分鉴权、限流、上游不可用和签名错误。网络超时后可重试原文；更改 Nonce、费率或授权后，须按账户规则重新签名。[RPC 错误定义](https://eips.ethereum.org/EIPS/eip-7769#eth_senduseroperation)

## 操作生命周期

下表描述服务内部的调度和对账过程。钱包使用上面的标准 RPC 查询操作和回执。

| 阶段 | 判断依据与下一步 |
|---|---|
| 验证 | 在有记录的区块状态下核对编码、EntryPoint 部署、账户签名验证、有效期、Gas 与费率、预付费用及验证规则。持久化成功后才返回操作哈希。验证失败返回具体错误。 |
| 入池 | 按链、EntryPoint 和操作哈希去重。重复提交已受理的相同操作返回原哈希。对同一发送方、同一完整 Nonce 的竞争操作串行处理。主动替换须重新签名，按配置提高两项费率上限并重新验证。 |
| 组包 | 按区块 Gas、calldata 大小和费用预算挑选相容操作。在当前链头重新验证每笔操作，核对全包代付余额，再模拟**整个 bundle**。发现不相容的操作时移出、重组并再次模拟，然后才签署。 |
| 提交与跟踪 | 独占外层发送账户的 Nonce，保存已签原文和包内操作清单，再提交 Broadcaster。保存其任务 ID 和各候选交易哈希。发送超时或实例重启时先用原哈希对账，再决定是否签署新包。 |
| 确认入块 | 在规范区块中按 EntryPoint 和操作哈希匹配 `UserOperationEvent`，用事件内的成功标志判断单笔结果。外层回退或 Nonce 被其他交易占用时，重新验证仍可执行的操作，并检查其他 Bundler 是否已将其打包入块。 |
| 确认或回查 | 回执绑定区块哈希与日志位置。达到配置的最终性条件前若发生重组，撤销孤块回执，从共同祖先重新索引，并重新验证未出现在新规范链上的操作。 |
| 结束调度 | 记录规范链执行结果、有效的签名替换、有效期届满或已核实的 Nonce 占用。保留历史哈希供业务对账。单次 RPC 无响应或本地等待超时后，链上执行结果仍需核查。 |

操作 Nonce 由 192 位 key 和 64 位序号组成。同一 key 内按序处理，并遵守操作池对单个发送方的接纳限制。外层签名账户使用独立的 EVM 交易 Nonce。Broadcaster 查询返回建议值；多实例组包仍需独占签名账户的 Nonce 并持久化组包记录。收到 Broadcaster 签名回调后唤醒核查。服务恢复时按原外层哈希补查，并从持久化的区块检查点重读 EntryPoint 日志。[Nonce 规范](https://eips.ethereum.org/EIPS/eip-4337#semi-abstracted-nonce-support) · [验证规则](https://eips.ethereum.org/EIPS/eip-7562)

## 生产部署与验收

| 项目 | 部署要求与验收依据 |
|---|---|
| 网络准入 | 核实 RPC 链身份、EntryPoint 字节码，用有效和无效操作验证模拟与追踪能力。检查日志查询范围、规范链历史、费率估算以及该链的 Broadcaster 准入。 |
| 并发与容量 | 按链和 EntryPoint 分配任务，限制模拟并发数、队列深度和每租户预算。达到最大等待时间时提交当前包，达到该链组包 Gas 预算时提前提交。突发写入时为回执读取保留容量。可复用的模拟结果绑定区块哈希和相关输入，状态变化时失效。 |
| 运营资金 | 各网络隔离签名账户，通过签名策略限定 EntryPoint、受益地址和外层费用上限。监控签名账户余额、代付占用和实际 Gas 收支。使用独立签名服务或 HSM 保管密钥。 |
| 持久化与恢复 | 多实例原子领取任务、替换操作、登记包内成员。保存索引检查点，备份并演练恢复。重启后从检查点补齐索引，再报告最新回执。 |
| 日常运维 | TLS 接入、按客户鉴权和限额，管理诊断接口仅供内部访问。日志关联 chain ID、EntryPoint、操作哈希、包交易哈希和区块哈希。监控索引停滞、模拟饱和、队列等待、签名账户余额及节点故障。 |

**已验证。** 本地 Anvil 与真实 Broadcaster 的集成测试覆盖同包自付成功、代付执行回退、保存签名后切换实例、提交响应丢失，以及重组后重新入块。内置验证器用于已审核合约的受管池。公开操作池的 ERC-7562 策略、上游规范套件以及生产网络容量需单独验收。

**上线验收。** 针对实际 EntryPoint 版本运行上游 Bundler 规范测试，用自付和代付操作完成端到端验证。覆盖重复提交、同 Nonce 替换竞争、签名实例切换、代付存款耗尽、节点已接收但响应丢失、同包部分操作回退，以及观察到回执后发生重组。复核重复通知和进程重启后同一已受理操作只产生一次付款。[Bundler 规范测试](https://github.com/eth-infinitism/bundler-spec-tests)

**性能报告。** 记录硬件、节点及客户端版本、目标链、区块 Gas 上限、追踪延迟、账户和代付场景占比、并发数及测试时长。分别报告每秒持久化受理数、每块实际上链操作数、受理和回执查询延迟的 p50/p95/p99、队列等待以及出块到回执可查的延迟。在稳定流量、突发流量和一家节点离线时分别测量。对外公布同时满足约定延迟和积压上限的最高持续处理速率。

## 许可证

公司及其关联实体过去 12 个月的全球合计收入严格超过 100 万美元，即使亏损也须购买许可。按社区条款，修改版在发布、交付或上线运行时须免费向公众提供该版本的完整对应源码，显著保留指定署名及原项目链接。每个修改版本（包括 SaaS 上线版本）还须按第 13 节通过 PR 或分支将该版本的实际修改提交给原项目，并注明提交号。GM Wallet 自行决定是否合并。另行签署的商业合同可约定不同条款。 [许可](LICENSE) · [info@gmwallet.app](mailto:info@gmwallet.app)
