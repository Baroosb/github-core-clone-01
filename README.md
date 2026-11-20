# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Baroosb/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Baroosb/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge implemented in Go (standard library only) and shipped as one statically compiled binary, `forge`. A thin `main.ts` launcher — the file the platform runs — resolves the binary beside it, spawns it, and mirrors its exit code; if the repository mount lost the binary's executable bit, the launcher copies it to a temporary directory and runs that copy instead. The binary itself owns the whole lifecycle and surface: it binds an ephemeral port, registers with the runner environment, and serves git smart HTTP (upload-pack/receive-pack), the REST v3 resources, GraphQL v4, and an MCP endpoint from its own in-memory object store — no `git` binary, no dependencies, no network.

## Run it

```sh
node main.ts
```

The forge binds an ephemeral port, registers itself with the runner environment, and serves until told to stop.

## Build

The committed `forge` binary is built from the Go sources in this repository (Go 1.23, module `forge`):

```sh
CGO_ENABLED=0 go build -ldflags "-s -w" -trimpath -o forge .
```

It is statically linked and stripped, so the tree ships exactly what runs: no build step, package manager, or toolchain is needed at run time.

## Layout

- `main.ts` — the launcher described above; the platform's entry point.
- `forge` — the compiled server (the Go sources below are its complete source).
- `main.go` — process entry: port binding, registration with the runner environment, routing.
- `githttp.go` — git smart HTTP: `info/refs`, `git-upload-pack`, `git-receive-pack`.
- `gitobj.go`, `store.go` — the git object model and the in-memory repository store.
- `rest.go` — the GitHub REST v3 surface.
- `graphql.go` — the GitHub GraphQL v4 schema and resolvers.
- `mcp.go` — the Model Context Protocol endpoint (initialize, tools/list, tools/call).

---

<sub>Entered on [make.fun](https://make.fun/?ref=5hnf181h&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
