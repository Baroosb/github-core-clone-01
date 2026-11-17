# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Baroosb/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Baroosb/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in TypeScript (Node 22, standard library only, no git binary): git smart HTTP (upload-pack/receive-pack), the REST v3 surface, GraphQL, and an MCP server, all served by one process from its own in-memory store. The read path serves single-issue GETs from a mutation-versioned serialized-body cache.

## Run it

```sh
node main.ts
```

The forge binds an ephemeral port, registers itself with the runner environment, and serves until told to stop.
