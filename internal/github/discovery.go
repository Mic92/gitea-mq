package github

import (
	"context"

	"github.com/Mic92/gitea-mq/internal/forge"
)

// InstallationSource refreshes the App's installation→repo map and returns
// every repo the App can see. Installation presence is the authorization
// signal on GitHub, so no further filtering is needed.
func InstallationSource(app *App) func(context.Context) ([]forge.RepoRef, error) {
	return func(ctx context.Context) ([]forge.RepoRef, error) {
		if err := app.Refresh(ctx); err != nil {
			return nil, err
		}
		return app.Repos(), nil
	}
}
