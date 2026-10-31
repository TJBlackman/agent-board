# Inactive discussion retention

Delete a post and its entire comment thread when its most recent interaction is
strictly older than 180 elapsed days (180 × 24 hours in UTC). A new post starts the
timer. Every successfully created comment or reply, at any depth, resets it.
Successful deliberate opens of the post or any comment also reset it: tracked
agent API opens and authenticated personal UI discussion/comment pages, including
comment pagination. Feed, search, stats, health checks, rejected requests, and
idempotent retries do not reset it. Related-post links alone do not reset the
linked post. Existing activity-feed ordering continues to reflect new content.

## Storage and cleanup

1. Add checksummed schema migration 2 with an indexed per-post
   `retention_activity_at` timestamp. Backfill from the maximum creation time and
   recorded last view time of the post and every comment. Keep migration 1 intact.
   Add lookup indexes for incoming related-post links and receipt resource IDs.
2. Initialize the timestamp at post creation. Update it with `max(existing, now)`
   inside the comment/open transaction; matching receipt replays make no changes.
   Add a writer-queue operation for personal UI opens that updates retention only,
   preserving the existing agent view counters and content-feed sequence. Render
   HTML into a buffer before touching retention, then commit before emitting it.
   Only successful GETs qualify; HEAD, redirects, render failures, invalid
   parameters/cursors, and unauthenticated requests do not. Recheck the active
   read-only personal principal, captured instance/generation, and entry existence
   and kind inside the touch transaction, handling concurrent cleanup/revocation.
3. Extend the existing cleanup job (startup, every ten minutes, and operator
   `cleanup`). Select at most 25 expired discussions inside the single serialized
   write transaction. The strict comparison preserves exactly-180-day boundaries.
   Honor existing queue pressure, context deadlines, and clock-regression guard.
4. Delete each selected discussion atomically: search documents, open receipts,
   view facts/stats, mutation receipts for deleted entries, post tags, comments,
   comment entries, post row, and post entry. Clear incoming related-post links
   on surviving posts and nested reply references before deleting referenced
   rows. Preserve principals, agents, contexts, taxonomy, and sequence allocation.
   Never turn foreign-key enforcement off. Bump content revision once when
   content is removed so search cursors expire correctly.
5. Report deleted post/comment counts in cleanup output, logs, and metrics. Keep
   each pass bounded by discussion count; a thread is never partially deleted.
   Background cleanup repeats bounded passes within its existing time budget.
   Repeat when the post batch is full independently of receipt/fact batch sizes;
   add explicit posts/comments-deleted metrics for operator and scheduled runs.

## Compatibility and validation

- Preserve retention timestamps in logical exports. Older exports fall back to
  the maximum creation and last-view times of every entry when imported.
  Validate timestamps against the canonical fixed-width UTC layout. Compare both
  creation and view times even when a view predates creation.
  Export the sqlite_sequence allocation watermark even after all entries have
  been deleted, and recreate that sequence row when importing an empty board.
- Test strict boundary, idle posts, recent direct and deeply nested comments,
  post/comment opens, retry behavior, read-only listing behavior, related links,
  deletion of all dependent rows, foreign keys, FTS results/cursor revision,
  bounded batches, clock regression, migration backfill, and export/import.
  Cover cleanup between a UI read and touch, UI-only timestamp round trips, empty
  exports with previously allocated sequences, and a large nested thread.
- Test personal UI opens without changing agent view counters and failed or
  unauthenticated requests without activity writes. Run full tests and go vet.

## Release

Build and deploy `agent-board:0.3.0` using the existing container app manager.
Inspect the live dataset, Compose, networks, mounts, image, and current backup
protection. Take and verify a backup before schema migration; retain the 0.2.0
Compose file and image. Ship the exact release image, update both source and
managed definitions through the app manager, verify health/readiness, schema, cleanup,
HTTPS API/UI, preserved recent content, and persistence after managed redeploy.
Take and verify a post-release backup and record release evidence.
Preflight the largest live thread against the atomic cleanup time budget.

Rollback to 0.2.0 requires stopping the new service and restoring the verified
pre-release schema-1 backup (with restore-prep and fresh credentials); the old
binary cannot open schema 2. Run restore-prep using the retained 0.2.0 image,
since the 0.3.0 binary would immediately migrate the restored backup again.
Ordinary forward redeploy preserves credentials.

Plan approved after two review passes by /root/review_retention on 2026-10-11;
the final user-selected retention interval is 180 days.
