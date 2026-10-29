//go:build !unix

package sqlite

import (
	"code-agent/internal/collab"
	"context"
)

func (*taskRepository) LockManagedHost(context.Context) (func(), error) {
	return nil, collab.ErrUnavailable
}
