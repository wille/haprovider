# haprovider

High Availability Provider is a validating and caching load balancer for Ethereum/L2, Bitcoin and Solana JSON-RPC nodes.

Running high availability production systems connecting to blockchains can be challenging. RPC providers can be unreliable, slow and rate limited.

## What it does better

- Move any fallback logic from your application layer and rely on one RPC endpoint
- Handles request failures and forwards to the next available provider
- Traffic observability
- Validating requests and responses and detect any error
- Detects rate limits and retries once the provider is available again
- ETH Transaction broadcasting
- Upstream keepalive/http2

## Configuration examples

A common example is to have a local Ethereum with a fallback to a third party provider.

```yml
providers:
  eth:
    kind: eth
    endpoints:
      - name: Local node
        http: http://localhost:8145
        ws: ws://localhost:8146
      - name: Infura
        http: https://mainnet.infura.io/v3/your-api-key
        ws: wss://mainnet.infura.io/ws/v3/your-api-key
```

> [!NOTE]
> You can use other providers (Alchemy, QuickNode, Cloudflare etc) and other chains like Polygon, Optimism etc

### One primary Solana RPC provider with a fallback

```yml
providers:
  solana:
    kind: solana
    endpoints:
      - name: quicknode
        http: https://haprovider.solana-mainnet.quicknode.pro/your-api-key
      - name: fallback
        http: https://solana.rpc.helius.network
```

### Bitcoin RPC fallback

```yml
providers:
  btc:
    kind: btc
    endpoints:
      - name: primary
        http: http://192.168.0.1:8332
      - name: fallback
        http: http://192.168.0.2:8332
```

> [!NOTE]
>
> For Bitcoin it's often important to be connected to one node and one node only, especially when doing wallet operations.
> There is a high risk of ending up with weird behaviour if the underlying Bitcoin node used by a service suddenly switches to another node.

## Features

### HTTP

You may have multiple HTTP providers for one endpoint.

- Supports HTTP and WebSockets
- Distribute traffic across multiple RPC providers

```ts
import Web3 from "web3";

const provider = new Web3("ws://localhost:8080");
```
