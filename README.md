# Prismaport

Get paid in stablecoins, straight to your wallet.

Prismaport is a non-custodial stablecoin payment gateway. Merchants accept USDC on Base with a link
or an API; every payment goes straight to the merchant's wallet, and the fee is split on-chain in
the same transaction. The hosted service runs at [prismaport.com](https://prismaport.com); this
repository is the published code of release 1.0.0, for reading.

## What is here

| Path | Contents |
|---|---|
| `web/checkout/` | The hosted checkout the payer sees (TypeScript, Vite) |
| `web/dashboard/` | The merchant dashboard (React, Vite) |
| `web/landing/` | The landing page |
| `web/docs/` | The developer portal at docs.prismaport.com (TypeScript, Vite); its tests check it against the Go code and the contract vectors |
| `docs/openapi.json` | The HTTP API contract (OpenAPI 3.1) |
| `cmd/prismaport/` | The server binary: the HTTP API, the chain watchers and the background workers |
| `internal/` | The Go packages behind it: API, checkout, chain scanning and settlement, webhooks, storage |
| `migrations/` | The Postgres schema migrations, embedded in the binary |
| `contracts/` | Submodule: [prismaport-contracts-evm-v1](https://github.com/prismaport/prismaport-contracts-evm-v1), the EVM contracts as deployed |
| `solana/` | Submodule: [prismaport-contracts-solana-v1](https://github.com/prismaport/prismaport-contracts-solana-v1), the Solana program |

The contracts live in their own repositories, one per deployed version, exactly as deployed. This
repository pins them as submodules because the Go and portal tests read their vectors, sources and
IDL.

## Running the tests

Clone with the submodules:

```sh
git clone --recurse-submodules https://github.com/prismaport/prismaport.git
```

Go (the version in `go.mod`):

```sh
go vet ./...
go test ./...
```

The integration tests need a Postgres server (17) and the `integration` build tag. They create
their own databases on it; point them at it with `PRISMAPORT_TEST_DATABASE_URL`:

```sh
PRISMAPORT_TEST_DATABASE_URL='postgres://user:pass@127.0.0.1:5432/postgres?sslmode=disable' \
  go test -tags integration ./...
```

The frontends and the portal, with Node 22 and npm 10:

```sh
cd web/checkout && npm ci && npm test
cd web/dashboard && npm ci && npm test
cd web/docs && npm ci && npm test
```

## License

MIT, see [LICENSE](LICENSE).
