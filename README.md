<p align="center"><img src="https://github.com/GMWalletApp.png?size=96" width="80" height="80" alt="GM Wallet"></p>
<h1 align="center">GM Wallet ERC-4337 Bundler</h1>
<p align="center"><strong>Submit a signed smart-account operation and follow its execution from admission to an on-chain receipt.</strong></p>
<p align="center"><strong>English</strong> · <a href="README.zh-CN.md">简体中文</a> · <a href="LICENSE">MIT</a></p>
<p align="center"><a href="#wallet-integration">Wallet integration</a> · <a href="#operation-lifecycle">Operation lifecycle</a> · <a href="#production-readiness">Production readiness</a></p>

**Project status.** This repository currently contains the design and license. The sections below define the intended behavior of the first implementation. A runnable service, verified network deployments and measured capacity are release deliverables.

## What the Bundler does

A wallet signs a `UserOperation` that describes what its smart account should do. The Bundler validates the operation, queues it and groups compatible operations into an EntryPoint call. An operator-funded signer pays gas for the outer transaction. EntryPoint accounts for each operation's gas against the account or its paymaster and pays the configured beneficiary. The wallet tracks its own operation by `userOpHash`. [ERC-4337](https://eips.ethereum.org/EIPS/eip-4337)

| Component | Responsibility |
|---|---|
| Wallet or merchant service | Construct account-specific calldata, estimate gas, obtain sponsorship when needed, sign the final operation and associate its hash with an order. |
| Paymaster | Decide sponsorship, supply any required paymaster data and signature, and maintain its EntryPoint deposit. |
| Bundler | Validate against a specific chain and EntryPoint, maintain the operation pool, select a bundle, request an outer signature, and index the outcome of each operation. |
| GM Wallet Broadcaster | Receive the **signed outer transaction** via `POST /EVM/transactions`. Schedule its sender nonce, fan out to available RPC providers, track replacements and notify the Bundler. [Broadcaster API](https://github.com/GMWalletApp/Broadcaster/blob/main/docs/api.md) |

**Compatibility target.** Start with an operator-managed pool and EntryPoint **v0.7** to match the EntryPoint configured in GM Wallet's Ethereum Broadcaster example. Pin the chain ID, address, runtime code hash, ABI and hashing rules together. Verify each deployment before advertising its address through RPC. v0.8/v0.9, EIP-7702 authorizations, signature aggregators and public mempool peering need separate compatibility tests before enablement. A chain listed by Broadcaster still needs a verified EntryPoint and simulation-capable RPC to run a Bundler. [GM Wallet configuration](https://github.com/GMWalletApp/Broadcaster/blob/main/config.example.json) · [EntryPoint releases](https://github.com/eth-infinitism/account-abstraction/releases)

## Wallet integration

The examples use curl and jq against a deployment you operate. Set `BUNDLER_RPC_URL` to the HTTPS URL for one chain and `BUNDLER_API_TOKEN` to the integration credential. The planned wire interface uses JSON-RPC 2.0 over HTTP POST. [ERC-7769](https://eips.ethereum.org/EIPS/eip-7769)

| Method | Parameters | Wallet action |
|---|---|---|
| `eth_chainId` | `[]` | Verify the chain ID before signing. |
| `eth_supportedEntryPoints` | `[]` | Choose an address supported by both your account implementation and this deployment. |
| `eth_estimateUserOperationGas` | Operation template, EntryPoint | Use the returned gas limits to complete the operation. Supply an account-compatible stub signature for estimation. |
| `eth_sendUserOperation` | Signed operation, EntryPoint | Save the returned hash. It acknowledges validated, durable admission. |
| `eth_getUserOperationByHash` | User operation hash | Retrieve the operation and its inclusion coordinates when available. A pending lookup can return null. |
| `eth_getUserOperationReceipt` | User operation hash | A null result means there is no inclusion receipt available. An object contains the operation's `success`, gas cost, logs and outer transaction receipt. |

### Example: submit one operation

First discover the chain and available EntryPoints. Select an address whose version matches the smart account. The remaining commands use that address in `ENTRY_POINT`.

```sh
rpc() {
  curl -fsS "$BUNDLER_RPC_URL" -H "Authorization: Bearer $BUNDLER_API_TOKEN" \
    -H 'Content-Type: application/json' --data-binary @-
}
jq -n '{jsonrpc:"2.0",id:1,method:"eth_chainId",params:[]}' | rpc
jq -n '{jsonrpc:"2.0",id:2,method:"eth_supportedEntryPoints",params:[]}' | rpc
```

Prepare a v0.7 RPC operation object in `operation.json`. Use hex quantities for the nonce, gas and fees, and hex bytes for calldata and signatures. For deployment, provide `factory` with `factoryData`. For sponsorship, include `paymaster`, `paymasterData`, `paymasterVerificationGasLimit` and `paymasterPostOpGasLimit`. The signer obtains the account nonce via `EntryPoint.getNonce(sender, key)`.

Example RPC shape for a deployed, self-funded account. Replace the example sender, nonce, calldata, fee values and stub signature with values for your account before estimation.

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

Apply the estimate and fee caps, finalize any sponsorship and sign with the account's signing scheme. Save the resulting RPC object as `signed-operation.json`. Send exactly that payload again if the first response is lost.

```sh
USER_OP_HASH=$(op_request eth_sendUserOperation signed-operation.json | rpc |
  jq -er 'if .error then error(.error|tostring) else .result end')
jq -n --arg hash "$USER_OP_HASH" \
  '{jsonrpc:"2.0",id:4,method:"eth_getUserOperationReceipt",params:[$hash]}' | rpc
```

A merchant settles an order from **that operation's** receipt after its chain confirmation policy is met. Two operations in one successful outer transaction can return different `success` values. Use `actualGasCost` for operation-level accounting. Preserve the raw reason when available if execution reverted.

### Admission errors

Expose structured JSON-RPC errors with the original EntryPoint `AAxx` reason where available. Invalid fields or unsupported EntryPoints use `-32602`. EntryPoint validation failure uses `-32500`, paymaster validation failure `-32501`, validation-rule violations `-32502`, an unsuitable validity window `-32503`, signature failure `-32507` and insufficient paymaster capacity `-32508`. API authentication, rate limits and upstream unavailability should be distinguishable from a failed signature. Retry a transport error with the original payload. A nonce, fee or authorization change requires a fresh signature under the account's rules. [RPC error definitions](https://eips.ethereum.org/EIPS/eip-7769#eth_senduseroperation)

## Operation lifecycle

The states below are internal scheduling and reconciliation states. Wallets use the standard RPC methods above.

| Stage | Decision and next step |
|---|---|
| Validate | Check the envelope, signature through account validation, supported deployment, validity window, gas and fee caps, prefund, and validation rules at a recorded block. Persist accepted operations before returning a hash. Return a structured error on rejection. |
| Queue | Scope deduplication to chain, EntryPoint and operation hash. An identical accepted submission returns its hash. Serialize competing operations for the same sender and full nonce. A signed replacement needs a configured increase in both fee caps and fresh validation. |
| Build | Select compatible operations under block gas, calldata and fee budgets. Revalidate each selection against the current head, check aggregate paymaster funding, then simulate the **whole bundle**. Remove a failing candidate, rebuild and re-simulate before signing. |
| Submit and track | Lease an outer signer nonce, persist signed bytes and the exact operation list, then submit to Broadcaster. Persist its task ID and every outer candidate hash. After a timeout or restart, reconcile that submission by hash before signing another bundle. |
| Index inclusion | Verify the canonical block and matching EntryPoint `UserOperationEvent` for each hash. Use its success flag for execution. If the outer transaction reverted or a candidate lost its nonce, revalidate operations still eligible for inclusion. Another bundler may already have included one. |
| Confirm or reopen | Keep the receipt tied to its block hash and log position. Before the configured finality threshold, an orphaned block invalidates its indexed receipts. Rescan from the common ancestor and revalidate operations missing from the new canonical chain. |
| Stop scheduling | Record the canonical outcome, signed replacement, expired validity window or verified nonce consumption. Keep historical hashes for reconciliation. A missing RPC response or a local queue timeout alone does not establish an on-chain execution failure. |

The sender's operation nonce contains a 192-bit key and a 64-bit sequence. Keep ordering within each key and apply the configured pool's admission limits per sender. The outer signer uses its own EVM transaction nonce. Broadcaster's nonce query is advisory. Concurrent builders need an exclusive lease per signer/nonce and a durable bundle record. A signed Broadcaster callback wakes reconciliation; following downtime, recover by querying the original outer hash and replaying EntryPoint logs from a durable block checkpoint. [Nonce support](https://eips.ethereum.org/EIPS/eip-4337#semi-abstracted-nonce-support) · [Validation rules](https://eips.ethereum.org/EIPS/eip-7562)

## Production readiness

| Area | Deployment design and release evidence |
|---|---|
| Network admission | Verify RPC chain identity, EntryPoint bytecode, simulation and tracing against known valid and invalid fixtures. Check log-range limits, canonical history access, fee estimation and Broadcaster admission on the target chain. |
| Capacity | Partition work by chain and EntryPoint. Bound simulation concurrency, queues and per-tenant budgets. Flush a partial bundle at the configured maximum queue wait or when its chain gas budget is filled. Reserve capacity for receipts when submission traffic spikes. Key any reusable simulation result by block hash and relevant inputs, and invalidate it when state changes. |
| Operator funds | Isolate signers per network, authorize only the intended EntryPoint and beneficiary, cap outer fees and monitor signer balance, paymaster exposure and realized gas margin. Keep signing keys in an external signer or HSM. |
| Recovery | Make claims, replacements and bundle membership atomic across replicas. Persist indexing checkpoints, back up the store and rehearse restore. Catch up from the checkpoint before reporting current receipts after a restart. |
| Service operation | Authenticate clients over TLS, enforce request and simulation quotas, and expose diagnostics privately. Correlate chain ID, EntryPoint, operation hash, bundle hash and block hash in traces. Alert on stalled indexing, simulation saturation, queue age, signer balance and provider failures. |

**Release tests.** Run the version-matched upstream Bundler tests and integration fixtures for sponsored and self-funded operations. Exercise duplicate submits, competing nonce replacements, signer failover, paymaster deposit depletion, accepted sends with lost replies, mixed execution results, and a reorg after a receipt has been observed. Verify duplicate deliveries and restarts do not create a second payment for one admitted operation. [Bundler specification tests](https://github.com/eth-infinitism/bundler-spec-tests)

**Performance report.** Publish hardware, node/client versions, chain, block gas limit, trace latency, account/paymaster mix, concurrency and test duration. Report admitted operations/s, on-chain operations/block, p50/p95/p99 admission and receipt-lookup latency, queue wait and block-to-index lag separately. Repeat at steady load, during bursts and with one provider unavailable. State the highest sustained rate that meets the deployment's agreed latency and backlog limits. These measurements establish the advertised service capacity.

## License

MIT permits commercial use subject to its notice requirements. The repository license text governs distribution. [MIT License](LICENSE)
