package forge

import (
	"context"
	"errors"
)

// ErrLabelsForbidden means the credentials may not manage labels.
var ErrLabelsForbidden = errors.New("forge: not permitted to manage labels")

// Labeler is optionally implemented by forges that can label PRs by name.
type Labeler interface {
	// ApplyLabels adds missing labels to the repo, then adds and removes them on one PR.
	ApplyLabels(ctx context.Context, owner, name string, number int64, add, remove []string) error
	// DeleteLabel removes the label from the repo and every PR; missing is not an error.
	DeleteLabel(ctx context.Context, owner, name, label string) error
}
