# ElecPostal

Go implementation of the Solar Network email service.

## Run

```bash
cp config.example.toml config.toml
# edit config.toml
go run ./cmd
```

## Logging

- `ZEROLOG_PRETTY=true` enables console-style pretty logs
- `LOG_LEVEL=debug|info|warn|error` sets the log level

## Config

Use `--config` or `CONFIG_PATH` for a TOML config file.

Key settings:

- `app.name`
- `database.dsn`
- `http.port`
- `grpc.port`
- `grpc.useTLS`
- `auth.target`
- `auth.useTLS`
- `solarNetwork.baseUrl`
- `solarNetwork.accessToken`
