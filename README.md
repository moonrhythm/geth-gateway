# geth-gateway

Gateway for multiple geth nodes

> gcr.io/moonrhythm-containers/geth-gateway

## Features

- Select highest block node when forward requests

## Config

| Flag | Type | Description | Default |
| --- | --- | --- | --- |
| -addr | string | HTTP listening address | :80 |
| -tls.addr | string | HTTPS listening address | |
| -tls.key | string | TLS private key file | |
| -tls.cert | stirng | TLS certificate file | |
| -upstream | string[] | Geth address | http://192.168.0.2,http://192.168.0.3:8545 |
| -healthy-duration | duration | Duration from last block that mark as healthy | 1m |

## License

MIT
