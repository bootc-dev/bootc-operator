# Agents

## Context

Read these docs before making changes:

- [docs/PRD.md](docs/PRD.md) — motivation, user stories, requirements
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — CRDs, components, design principles
- [docs/IMPLEMENTATION_PLAN.md](docs/IMPLEMENTATION_PLAN.md) — milestones and validation steps

## Building and Testing

- `make build` — build all binaries
- `make unit` — run unit tests (includes envtest setup, CRD generation). `V=1` for verbose. `RUN=<regex>` to filter.
- `make deploy-bink` — deploy operator to a bink cluster (idempotent; requires `make buildimg` first)
- `make teardown-bink` — tear down the bink cluster
- `make e2e` — run e2e tests (requires `make deploy-bink` first). `V=1` for verbose streaming output. `RUN=<regex>` to filter.
- `make fmt` — run go fmt
- `make vet` — run go vet
- `make lint` — run golangci-lint
