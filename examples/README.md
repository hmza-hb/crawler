# Examples

| Path | Description |
|------|-------------|
| [basic](basic/main.go) | Single-process crawl with `MemoryFrontier` and `MemoryStore` |

```bash
go run ./examples/basic https://example.com/
```

For production-style distributed crawling, run `crawld` with PostgreSQL (`cmd/crawld`).
