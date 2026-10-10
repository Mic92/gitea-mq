package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

const (
	labelWaiting = "waiting"
	labelQueued  = "queued"
	labelTesting = "testing"
	labelMerging = "merging"
	labelBatch   = "batch-"
)

// liveBatches filters out cancelled batches whose entries keep their active_batch_id.
func desiredStateLabel(entry *pg.QueueEntry, intent bool, liveBatches map[int64]bool) string {
	if entry == nil {
		if intent {
			return labelWaiting
		}
		return ""
	}
	if entry.ActiveBatchID.Valid && liveBatches[entry.ActiveBatchID.Int64] {
		return fmt.Sprintf("%s%d", labelBatch, entry.ActiveBatchID.Int64)
	}
	switch entry.State {
	case pg.EntryStateTesting:
		return labelTesting
	case pg.EntryStateSuccess:
		return labelMerging
	default:
		return labelQueued
	}
}

// syncStateLabels diffs against the labels already on each PR, so a settled queue costs no API calls.
func syncStateLabels(ctx context.Context, deps *Deps, result *PollResult, openPRs []forge.PR) {
	if deps.LabelPrefix == "" || deps.labelsDisabled {
		return
	}
	labeler, ok := deps.Forge.(forge.Labeler)
	if !ok {
		return
	}

	entries, err := deps.Queue.ListActiveEntries(ctx, deps.RepoID)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("list entries for labels: %w", err))
		return
	}
	byPR := make(map[int64]*pg.QueueEntry, len(entries))
	for i := range entries {
		byPR[entries[i].PrNumber] = &entries[i]
	}
	liveBatches := map[int64]bool{}
	if deps.Batch.Enabled() {
		bs, err := deps.Queue.ListLiveBatches(ctx, deps.RepoID)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("list batches for labels: %w", err))
			return
		}
		for _, b := range bs {
			liveBatches[b.ID] = true
		}
	}

	if deps.stateLabels == nil {
		deps.stateLabels = map[int64]string{}
	}
	batchPrefix := deps.LabelPrefix + labelBatch
	seenBatch := map[string]bool{}
	wantBatch := map[string]bool{}
	open := make(map[int64]bool, len(openPRs))

	for i := range openPRs {
		pr := &openPRs[i]
		open[pr.Number] = true
		intent := pr.AutoMergeEnabled || pr.HasLabel(deps.MergeLabel)
		want := desiredStateLabel(byPR[pr.Number], intent, liveBatches)
		if want != "" {
			want = deps.LabelPrefix + want
		}
		if strings.HasPrefix(want, batchPrefix) {
			wantBatch[want] = true
		}

		var have []string
		for _, l := range pr.Labels {
			if hasPrefixFold(l, deps.LabelPrefix) {
				have = append(have, l)
				if hasPrefixFold(l, batchPrefix) {
					seenBatch[l] = true
				}
			}
		}
		deps.stateLabels[pr.Number] = ""
		if len(have) == 1 && want != "" && strings.EqualFold(have[0], want) {
			deps.stateLabels[pr.Number] = want
			continue
		}
		if want == "" && len(have) == 0 {
			delete(deps.stateLabels, pr.Number)
			continue
		}
		var add, remove []string
		if want != "" {
			add = []string{want}
		}
		for _, l := range have {
			if !strings.EqualFold(l, want) {
				remove = append(remove, l)
			}
		}
		if !applyStateLabels(ctx, deps, result, labeler, pr.Number, add, remove) {
			if deps.labelsDisabled {
				return
			}
			continue
		}
		if want == "" {
			delete(deps.stateLabels, pr.Number)
		} else {
			deps.stateLabels[pr.Number] = want
		}
	}

	// Merged/closed PRs are no longer listed; clean up what we set.
	for n, l := range deps.stateLabels {
		if open[n] {
			continue
		}
		// Batch labels are deleted with their batch.
		if l != "" && !hasPrefixFold(l, batchPrefix) {
			if !applyStateLabels(ctx, deps, result, labeler, n, nil, []string{l}) {
				if deps.labelsDisabled {
					return
				}
				continue
			}
		}
		delete(deps.stateLabels, n)
	}

	// One delete detaches a batch label from every PR.
	for l := range seenBatch {
		if wantBatch[l] {
			continue
		}
		if err := labeler.DeleteLabel(ctx, deps.Owner, deps.Repo, l); err != nil {
			if handleLabelErr(deps, err) {
				return
			}
			result.Errors = append(result.Errors, fmt.Errorf("delete label %s: %w", l, err))
		}
	}
}

func applyStateLabels(ctx context.Context, deps *Deps, result *PollResult, l forge.Labeler, number int64, add, remove []string) bool {
	remove = filterOut(remove, deps.LabelPrefix+labelBatch)
	if len(add) == 0 && len(remove) == 0 {
		return true
	}
	err := l.ApplyLabels(ctx, deps.Owner, deps.Repo, number, add, remove)
	if err == nil {
		return true
	}
	if !handleLabelErr(deps, err) {
		result.Errors = append(result.Errors, fmt.Errorf("label PR #%d: %w", number, err))
	}
	return false
}

// handleLabelErr disables labelling after a permission error and reports whether it did.
func handleLabelErr(deps *Deps, err error) bool {
	if !errors.Is(err, forge.ErrLabelsForbidden) {
		return false
	}
	deps.labelsDisabled = true
	slog.Warn("not permitted to manage labels, queue state labels disabled for this repo",
		"owner", deps.Owner, "repo", deps.Repo, "error", err)
	return true
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func filterOut(labels []string, prefix string) []string {
	var out []string
	for _, l := range labels {
		if !hasPrefixFold(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}
