# Deploying the v0.7 Bundler

[English README](../README.md) · [中文部署说明](operations.zh-CN.md)

## Configure one deployment

Copy [config.example.json](../config.example.json) into a private config file. Choose one EIP-1559 chain, EntryPoint v0.7 deployment, funded outer sender and reimbursement beneficiary. Check the EntryPoint runtime code hash against your approved contract release. Use a nonnegative `startBlock` at or before the first operation you need to index. The startup check pins network identity and deployed code. Every replica sharing the database must use the same deployment identity and start height.

| Setting | Operator supplies |
|---|---|
| `dbDriver`, `dbEnv` | For multiple hosts use `pgx` with a PostgreSQL primary or HA primary endpoint. The named variable contains its database URL. PostgreSQL HA, backups and failover are deployment dependencies. For local testing set `sqlite` and use a local file DSN with `?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate`. |
| `executionEnv`, `backupExecutionEnvs` | Environment variable names for independent read RPCs. Each endpoint needs canonical block/hash reads, transaction receipts, log filters, fee estimates, and Geth-compatible `debug_traceCall`. The built-in simulator also needs state overrides. A failed reader is tried on another verified endpoint. |
| `simulationArtifact`, `approvedValidationCode` | For the built-in managed pool, install the exact v0.7 simulations artifact below. List Keccak-256 runtime hashes of reviewed validation contracts, including account proxies, implementations, factories and paymasters in use. Review code paths, upgrade authority, storage access and applicable ERC-7562 rules before approving them. This mode is intended for controlled accounts and sponsors. |
| `validationEnv` | Optional private validation service. When set, it supplies estimation and block-bound validation through the adapter contract below. Use this to integrate your full public-mempool policy. |
| `signerEnv` | Private signer RPC supporting `eth_signTransaction` for a type-2 envelope. Configure chain, permitted EntryPoint, beneficiary and fee budget in signer policy. Recovered sender and unsigned transaction digest are checked locally on every response. |
| `broadcasterEnv`, `broadcastTokenEnv` | Base URL and business token for GM Wallet Broadcaster. Admit at least one RPC there and provision the submitting client for the same network. |
| `webhookSecretEnv`, `clientTokenEnvs` | Configure Broadcaster's callback to `https://<bundler>/webhooks/broadcaster` with the same HMAC secret. Use at least 24 bytes per wallet token and webhook secret. Expose wallet RPC through TLS; restrict signer and provider endpoints privately. |
| `poolSize`, `maxOps`, `maxBytes`, `maxGas`, `maxFeeWei`, `pollMillis` | Bound pool size, batch size, encoded calldata, outer gas, signer exposure and polling interval. The example allows 16 operations per bundle. One failed RPC does not invalidate a signature. |
| `confirmations` | Depth used to mark an indexed operation `FINAL`, counting the inclusion block. A later change to canonical history triggers replay. Configure settlement according to the target chain. |

Use the artifact from the immutable npm release. Its digest is checked again by the executable. [Official contracts package](https://www.npmjs.com/package/@account-abstraction/contracts/v/0.7.0)

```sh
curl -fsSL 'https://registry.npmjs.org/@account-abstraction/contracts/-/contracts-0.7.0.tgz' |
  tar -xzO package/artifacts/EntryPointSimulations.json > EntryPointSimulations.json
go build -o bin/bundler ./cmd/bundler
./bin/bundler -config config.json
```

## Lifecycle and recovery

| Observable state | What advances it |
|---|---|
| `QUEUED` | Simulation passed at a recorded chain head. The operation is durable. A queued operation may be explicitly replaced by a valid signed candidate raising both fee caps by at least 10%. |
| `BUNDLED` with outer `SIGNED` | The current worker revalidated selected operations, simulated the whole call, verified the signed bytes and atomically committed bundle membership. Any replica can take over sending these exact bytes. |
| Outer `SUBMITTED` | Broadcaster acknowledged persistence. Check `broadcast.attempts` for individual outer candidates. A timeout triggers lookup by the original transaction hash. A fee-bump request can produce a capped, same-nonce replacement persisted before submission. |
| `INCLUDED` | A matching event in a canonical EntryPoint receipt supplies the operation's execution result. Successful and reverted operations can share the same outer receipt. |
| `FINAL` | The configured depth was reached. Every lookup checks the receipt's canonical block. Reorg recovery drops orphaned receipts and revalidates displaced operations. |
| `REPLACED`, `CONSUMED`, `EXPIRED`, `REJECTED` | A newer operation occupied the local slot, a different operation consumed the slot on chain, the validity window expired, or deterministic revalidation failed. The status query includes the recorded reason. |

`gm_getUserOperationStatus([userOpHash])` exposes these states for operations known to this service. Standard wallet methods retain their ERC-7769 response shapes. A Broadcaster callback verifies HMAC-SHA256 over the raw body and wakes a durable task. Receipt indexing and polling also run when callback delivery is interrupted. Replicas coordinate the builder, delivery worker and indexer using independent database-clock leases. Signed network calls may repeat; only the current lease holder can commit a transition.

## Implementing company services

| Go interface | Contract |
|---|---|
| `Signer` | Implement `Sign(ctx, unsignedTx, from)` in your KMS/HSM adapter. Return signed bytes without broadcasting. The included RPC adapter accepts a hex envelope or an object with `raw`. |
| `Validator` | Implement `Estimate` and `Validate`. The optional remote adapter calls `eth_estimateUserOperationGas(op, entryPoint)` and private `gm_validateUserOperation(op, entryPoint, blockHash)`. Validation returns `blockHash`, hex `prefund`, hex `paymasterDeposit` for sponsored operations, and integer Unix timestamps `validAfter` and `validUntil` (zero means unbounded). Check signatures, validity, stake, storage and opcode rules. Return structured RPC errors for deterministic refusals. |
| `Broadcaster` | The included HTTP client implements nonce advice, hash lookup, submit and explicit same-nonce replacement using the `/EVM` namespace. Use one authenticated Broadcaster identity across replicas. |

## Running and measuring

Use `GET /ready` for read readiness. It returns 503 until the stored checkpoint reaches the current canonical head or its parent. Authenticated `GET /metrics` exports queue age and size, index height, worker errors and latency histograms. Monitor signer balance, database health, provider latency and treasury margin at the deployment boundary. Receipt queries do not consume simulation slots.

```sh
go test -race -shuffle=on ./...
go vet ./...
```

The optional end-to-end test starts its own loopback Anvil and Broadcaster processes. Provide the Anvil binary (tested with Foundry 1.7.1), a Broadcaster binary (tested at `a499e08`) and the official v0.7 npm `package/artifacts` directory. Set `TEST_PG_DSN` to a disposable PostgreSQL URL to test replica takeover there; the test creates and removes its own schema. Otherwise it uses SQLite. Set `TEST_PROFILE=1` for a 48-operation, six-client burst with measured admission and receipt latency.

```sh
TEST_ANVIL=/path/to/anvil TEST_BROADCASTER=/path/to/broadcaster \
  TEST_AA_ARTIFACTS=/path/to/package/artifacts \
  go test -count=1 -v -run TestAnvilBroadcaster ./bundler
```

Release qualification remains per deployment. Exercise steady traffic, bursts, provider loss and database failover with the production signer and node fleet. Run the version-matched compatibility suite for the chosen policy before advertising support for a public pool. [Upstream v0.7 suite](https://github.com/eth-infinitism/bundler-spec-tests/tree/releases/v0.7)
