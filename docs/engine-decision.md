# Engine decision

Date: 2026-10-07. Spike source: `spike/sqlite/main.go` and `spike/turso/main.go`. Run in WSL Ubuntu with Go 1.27.1 linux/amd64.

## Verdict

SQLite through `modernc.org/sqlite` v1.60.1 (bundled SQLite 3.53.4, pure Go, no CGO) is the v1 engine. Turso fails the mandatory search gate.

## Turso (github.com/tursodatabase/turso-go v0.2.2, engine 0.2.0)

| Gate | Result |
| --- | --- |
| Opens a persistent file with CGO disabled | Pass |
| AUTOINCREMENT table creation | Pass |
| Unique conflict handling | Fail. The driver returns the opaque string `error executing statement`, so the service cannot distinguish a UUID collision from any other constraint violation. |
| Full text search | Fail. `create virtual table ... using fts5` returns `no such module: fts5`; the documented experimental `fts_create_index` and `fts_match` functions return `no such function`. The Go distribution ships without any FTS. |

With no search engine and no usable constraint errors, the remaining gates were not run.

## SQLite (modernc.org/sqlite v1.60.1)

| Gate | Result |
| --- | --- |
| CGO free build, Linux and Windows | Pass (`CGO_ENABLED=0 go build`) |
| WAL, synchronous FULL, foreign keys ON, busy_timeout via DSN `_pragma` | Pass (`journal_mode=wal foreign_keys=1`) |
| `ENABLE_FTS5` in compile options | Pass |
| AUTOINCREMENT never reuses a deleted maximum | Pass (deleted 3, next insert got 4) |
| Unique and foreign key violations distinguishable | Pass (extended codes 2067 and 787 in the error text) |
| FTS5 with explicit rowid, `bm25()` with column weights 3/2/1 | Pass. Lower (more negative) bm25 is the better match; the service orders `score ASC`. |
| Grouping best document per post with `row_number()` window | Pass |
| Phrase queries and hyphenated identifiers under `unicode61 remove_diacritics 2` | Pass. `"error-code"` and `"s3-client"` match as phrases; `s3` matches `s3-client` because the tokenizer splits on the hyphen. |
| Rolled back insert never visible to search | Pass |
| Statement cancellation reaches the engine | Pass. A cross join over 200k rows stopped 2 ms after a 50 ms context deadline. |
| Consistent online backup with `VACUUM INTO` | Pass. The copy passed `integrity_check` and answered FTS queries. |
| 10 concurrent readers plus 10 concurrent writers with 1 s busy timeout | Pass, zero failures |

## Durability settings in production

```text
journal_mode=WAL  synchronous=FULL  foreign_keys=ON  busy_timeout=1000
one writer connection owned by the writer queue
read pool capped at 16 connections
```

## Tokenizer profile (documented in the root guide)

`unicode61 remove_diacritics 2`. Case folding and diacritic removal apply. Punctuation and hyphens split tokens. No stemming, no substring matching. A quoted phrase requires consecutive tokens in one field.

## Operator access while the service runs

All online operator actions (token create, list, revoke, backup now, search rebuild, metrics) go through the admin listener bound to loopback inside the container. Offline commands (`export`, `import`) open the database directly and require the service to be stopped; `import` refuses to run against a non empty target.
