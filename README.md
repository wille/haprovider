# haprovider

[![Go Report Card](https://goreportcard.com/badge/github.com/wille/haprovider)](https://goreportcard.com/report/github.com/wille/haprovider)
[![GoDoc](https://godoc.org/github.com/wille/haprovider?status.svg)](https://godoc.org/github.com/wille/haprovider)
[![License](https://img.shields.io/github/license/wille/haprovider)](LICENSE)

*High Availability Provider* is a load balancer for Ethereum and Solana JSON-RPC nodes, designed to provide reliable and efficient access to blockchain networks.

## Table of Contents
- [Features](#features)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [Configuration](#configuration)
- [Examples](#examples)
- [Monitoring](#monitoring)
- [Troubleshooting](#troubleshooting)
- [Contributing](#contributing)

## Features

- **High Availability**: Move any fallback logic from your application layer and rely on one RPC endpoint
- **Automatic Failover**: Handles request failures and forwards to the next available node or node provider
- **Traffic Observability**: Monitor and analyze your RPC traffic patterns
- **Request Validation**: Validates requests and responses to detect errors
- **Rate Limit Handling**: Detects rate limits and retries once the provider is available again
- **Connection Optimization**: Upstream keepalive/http2 connection pooling
- **Health Checks**: Automatic health monitoring of all configured providers

## Installation

```bash
# Using Go
go install github.com/wille/haprovider

# Using Docker
docker pull ghcr.io/wille/haprovider
```

## Quick Start

1. Create a configuration file `config.yml`:
```yml
endpoints:
  ethereum:
    kind: eth
    chainId: 1
    providers:
      - name: Local node
        http: http://localhost:8145
        ws: ws://localhost:8146
      - name: backup
        http: https://eth.llamarpc.com
```

2. Start the service:
```bash
$ haprovider --config config.yml
```

3. Connect your application:
```typescript
import { ethers } from 'ethers';

// ethers.js v6
const provider = new ethers.JsonRpcProvider('http://localhost:8080/eth');
// or
const provider = new ethers.WebSocketProvider('ws://localhost:8080/eth');
```

## Configuration

The configuration file supports the following options:

```yml
# Endpoint configurations
endpoints:
  ethereum:
    kind: eth
    chainId: 1
    providers:
      - name: Local node
        http: http://localhost:8145
        ws: ws://localhost:8146
        timeout: 10s
      - name: Infura
        http: https://mainnet.infura.io/v3/<api-key>
        ws: wss://mainnet.infura.io/ws/v3/<api-key>
  solana:
    kind: solana
    providers:
      - name: quicknode
        http: https://solana-mainnet.quicknode.pro/<api-key>
```

### Configuration Options

- `port`: HTTP/WS server port (default: 8080) (can be set with $HA_PORT)
- `log_level`: Logging level (debug, info, warn, error) (can be set with $HA_LOGLEVEL)
- `log_json`: Enable JSON logs (can be set with $HA_JSON)
- `endpoints`: Map of endpoint configurations
  - `kind`: Provider type (eth, solana)
  - `chainId`: Network chain ID (optional, Ethereum only)
  - `providers`: List of provider configurations
    - `name`: Provider identifier
    - `http`: HTTP endpoint URL
    - `ws`: WebSocket endpoint URL (optional)
    - `timeout`: Request timeout (optional, default 10s)

## Examples

### Ethereum with Multiple Providers

```yml
endpoints:
  ethereum:
    kind: eth
    chainId: 1
    providers:
      - name: Local node
        http: http://localhost:8145
        ws: ws://localhost:8146
      - name: Infura
        http: https://mainnet.infura.io/v3/<api-key>
        ws: wss://mainnet.infura.io/ws/v3/<api-key>
      - name: QuickNode
        http: https://mainnet.quicknode.pro/<api-key>
        ws: wss://mainnet.quicknode.pro/<api-key>
```

### Solana Configuration

```yml
endpoints:
  solana:
    kind: solana
    providers:
      - name: quicknode
        http: https://solana-mainnet.quicknode.pro/<api-key>
      - name: fallback
        http: https://solana.rpc.helius.network
```

## Monitoring

haprovider exposes Prometheus metrics for monitoring:

- `haprovider_requests_total`: Total number of requests
- `haprovider_request_duration_seconds`: Request duration histogram
- `haprovider_provider_health`: Provider health status
- `haprovider_provider_errors_total`: Total provider errors
- `haprovider_rate_limits_total`: Rate limit events

Access metrics at `http://localhost:9090/metrics`

## Troubleshooting

Common issues and solutions:

1. **Connection Timeouts**
   - Check provider URLs and network connectivity
   - Adjust timeout settings in configuration

2. **Rate Limiting**
   - Monitor rate limit metrics
   - Consider adding more providers or upgrading API tiers

3. **Health Check Failures**
   - Verify provider endpoints are accessible
   - Check provider status pages

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add some amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
