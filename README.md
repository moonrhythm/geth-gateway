# geth-gateway

Gateway for multiple geth nodes

> gcr.io/moonrhythm-containers/geth-gateway

## Features

- Select highest block node when forward requests

## Config

| Flag                            | Type     | Description                                                   | Default |
|---------------------------------|----------|---------------------------------------------------------------|---------|
| -addr                           | string   | HTTP listening address                                        | :80     |
| -tls.addr                       | string   | HTTPS listening address                                       |         |
| -tls.key                        | string   | TLS private key file                                          |         |
| -tls.cert                       | stirng   | TLS certificate file                                          |         |
| -upstream                       | string[] | Geth address (ex. http://192.168.0.2,http://192.168.0.3:8545) |         |
| -healthy-duration               | duration | Duration from last block that mark as healthy                 | 1m      |
| -health-check.deadline          | duration | Deadline when run health check                                | 4s      |
| -health-check.interval          | duration | Health check interval                                         | 2s      |
| -fallback.upstream              | string[] | Fallback upstream list                                        |         |
| -fallback.health-check.deadline | duration | Fallback deadline when run health check                       | 5s      |
| -fallback.health-check.interval | duration | Fallback health check interval                                | 5s      |
| -retry.max                      | int      | Max retry count                                               | 3       |
| -retry.backoff                  | duration | Backoff duration                                              | 10ms    |

## License

MIT
