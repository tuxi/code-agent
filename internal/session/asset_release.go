package session

import (
	"context"
	"time"
)

type AssetRefRelease struct {
	SessionID       string
	CredentialScope string
	Attempts        int
	NextAttemptAt   time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type AssetRefReleaseStore interface {
	EnqueueAssetRefRelease(ctx context.Context, release AssetRefRelease) error
	DeleteWithAssetRefRelease(ctx context.Context, sessionID string, release AssetRefRelease) error
	PendingAssetRefReleases(ctx context.Context, credentialScope string, now time.Time) ([]AssetRefRelease, error)
	RetryAssetRefRelease(ctx context.Context, sessionID string, attempts int, nextAttempt time.Time) error
	CompleteAssetRefRelease(ctx context.Context, sessionID string) error
}
