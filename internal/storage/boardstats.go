package storage

import (
	"context"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
)

// WebIdentity is current authorization and board epoch, read in one statement.
type WebIdentity struct{ PrincipalID, InstanceID, GenerationID string }

func (db *DB) LoadWebIdentity(ctx context.Context, principalID string) (WebIdentity, error) {
	var v WebIdentity
	err := db.reads.QueryRowContext(ctx, `SELECT p.id, b.instance_id, b.generation_id
FROM api_principals p CROSS JOIN board_state b
WHERE p.id=? AND p.permission='read' AND p.revoked_at IS NULL AND b.id=1`, principalID).
		Scan(&v.PrincipalID, &v.InstanceID, &v.GenerationID)
	return v, err
}

type CreationCounts struct{ Posts, Comments int64 }
type BoardStats struct {
	AsOf                                        time.Time
	AllTime, Last24Hours, Last7Days, Last30Days CreationCounts
}

// LoadBoardStats counts canonical creation times in rolling [start, asOf) windows.
// asOf is passed explicitly so all windows share an instant and boundary tests are deterministic.
func (db *DB) LoadBoardStats(ctx context.Context, asOf time.Time) (*BoardStats, error) {
	s := &BoardStats{AsOf: asOf.UTC()}
	err := db.reads.QueryRowContext(ctx, `SELECT
count(*) FILTER (WHERE kind='post'), count(*) FILTER (WHERE kind='comment'),
count(*) FILTER (WHERE kind='post' AND created_at>=?1 AND created_at<?4),
count(*) FILTER (WHERE kind='comment' AND created_at>=?1 AND created_at<?4),
count(*) FILTER (WHERE kind='post' AND created_at>=?2 AND created_at<?4),
count(*) FILTER (WHERE kind='comment' AND created_at>=?2 AND created_at<?4),
count(*) FILTER (WHERE kind='post' AND created_at>=?3 AND created_at<?4),
count(*) FILTER (WHERE kind='comment' AND created_at>=?3 AND created_at<?4)
FROM entries`, domain.FormatTime(asOf.Add(-24*time.Hour)), domain.FormatTime(asOf.Add(-168*time.Hour)), domain.FormatTime(asOf.Add(-720*time.Hour)), domain.FormatTime(asOf)).
		Scan(&s.AllTime.Posts, &s.AllTime.Comments, &s.Last24Hours.Posts, &s.Last24Hours.Comments, &s.Last7Days.Posts, &s.Last7Days.Comments, &s.Last30Days.Posts, &s.Last30Days.Comments)
	if err != nil {
		return nil, err
	}
	return s, nil
}
