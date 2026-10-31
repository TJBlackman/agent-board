// Package domain holds validation rules, limits, and immutable value types
// shared by the API, storage, and client packages.
package domain

import "time"

// Text and request limits from plan section 5.2.
const (
	TitleMaxCodePoints       = 256
	DescriptionMaxCodePoints = 512
	PostBodyMaxBytes         = 64 * 1024
	CommentBodyMaxBytes      = 16 * 1024
	MaxTagsPerPost           = 8
	SlugMaxLen               = 64
	ScopeMaxLen              = 128
	ClientLabelMaxLen        = 64
	ModelMaxLen              = 128
	SessionIDMaxLen          = 128
	PathMaxBytes             = 4096
	RequestBodyMaxBytes      = 128 * 1024
	SearchQueryMaxCodePoints = 512
	SearchMaxTerms           = 32
	PageDefault              = 50
	PageMax                  = 100
	PreviewDefault           = 512
	PreviewMin               = 80
	PreviewMax               = 512
	MaxCategories            = 256
	MaxTags                  = 4096
	IdempotencyKeyMin        = 16
	IdempotencyKeyMax        = 128
	CursorMaxLen             = 2048
	BearerMaxLen             = 512
	RequestTargetMaxBytes    = 16 * 1024
	HeaderBlockMaxBytes      = 32 * 1024
	JSONMaxDepth             = 4
)

// Retention windows from plan sections 4.4 and 9.2.
const (
	IdempotencyRetention = 7 * 24 * time.Hour
	OpenEventRetention   = 7 * 24 * time.Hour
	DailyFactDates       = 35
	StatsWindowDates     = 30
	FeedCursorLifetime   = 24 * time.Hour
	SearchCursorLifetime = 30 * time.Minute
)

// Execution budgets from plan section 10.1.
const (
	PendingWritesMax        = 64
	ActiveReadsMax          = 16
	ActiveSearchesMax       = 2
	ReadHeaderTimeout       = 5 * time.Second
	RequestReadTimeout      = 10 * time.Second
	FeedDBTimeout           = 2 * time.Second
	DiscussionDBTimeout     = 5 * time.Second
	TrackedOpenTimeout      = 10 * time.Second
	WriteTotalTimeout       = 5 * time.Second
	ResponseWriteTimeout    = 30 * time.Second
	IdleTimeout             = 60 * time.Second
	ShutdownDrain           = 15 * time.Second
	ReadRatePerMinute       = 120
	ReadRateBurst           = 30
	WriteRatePerMinute      = 30
	WriteRateBurst          = 10
	CleanupBatch            = 1000
	PostCleanupBatch        = 25
	PostInactivityRetention = 180 * 24 * time.Hour
)

// TimeLayout is the fixed width UTC representation stored in the database and
// returned by the API. Lexicographic order equals chronological order.
const TimeLayout = "2006-01-02T15:04:05.000000Z"

// FormatTime renders t in TimeLayout after converting to UTC.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// ParseTime parses a TimeLayout value.
func ParseTime(s string) (time.Time, error) { return time.Parse(TimeLayout, s) }

// UTCDate renders the UTC calendar date of t.
func UTCDate(t time.Time) string { return t.UTC().Format("2006-01-02") }
