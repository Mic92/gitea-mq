// Package metrics exposes Prometheus metrics. Event counters are process-local
// and reset on restart; queue and batch gauges are read from the database on
// every scrape so they stay correct across restarts and replicas.
package metrics

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

const namespace = "gitea_mq"

// Reason labels why a PR left the queue.
type Reason string

const (
	ReasonMerged         Reason = "merged"
	ReasonClosed         Reason = "closed"
	ReasonRetargeted     Reason = "retargeted"
	ReasonPushed         Reason = "pushed"
	ReasonCancelled      Reason = "cancelled"
	ReasonDraft          Reason = "draft"
	ReasonBlocked        Reason = "blocked"
	ReasonUnreviewed     Reason = "unreviewed"
	ReasonCheckFailed    Reason = "check_failed"
	ReasonTimeout        Reason = "timeout"
	ReasonSuccessTimeout Reason = "success_timeout"
	ReasonConflict       Reason = "conflict"
	ReasonError          Reason = "error"
)

var repoLabels = []string{"forge", "owner", "repo"}

var (
	enqueued = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "prs_enqueued_total",
		Help:      "PRs added to the merge queue.",
	}, repoLabels)

	dequeued = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "prs_dequeued_total",
		Help:      "PRs removed from the merge queue, by reason.",
	}, append(repoLabels, "reason"))

	mergedDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "merged_pr_queue_duration_seconds",
		Help:      "Time from enqueue to merge for merged PRs.",
		Buckets:   []float64{60, 300, 600, 1200, 1800, 3600, 7200, 14400, 43200, 86400},
	}, repoLabels)

	batchBuilds = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "batch_builds_total",
		Help:      "Batch branches built for CI, including bisection rebuilds.",
	}, repoLabels)
)

// Enqueued records a PR entering the queue.
func Enqueued(kind forge.Kind, owner, repo string) {
	enqueued.WithLabelValues(string(kind), owner, repo).Inc()
}

// Dequeued records entry leaving the queue. Merged entries also feed the
// time-in-queue histogram.
func Dequeued(kind forge.Kind, owner, repo string, entry *pg.QueueEntry, reason Reason) {
	dequeued.WithLabelValues(string(kind), owner, repo, string(reason)).Inc()
	if reason == ReasonMerged && entry.EnqueuedAt.Valid {
		mergedDuration.WithLabelValues(string(kind), owner, repo).
			Observe(time.Since(entry.EnqueuedAt.Time).Seconds())
	}
}

// BatchBuilt records a batch branch build.
func BatchBuilt(kind forge.Kind, owner, repo string) {
	batchBuilds.WithLabelValues(string(kind), owner, repo).Inc()
}

// StatsSource supplies the per-scrape queue snapshot.
type StatsSource interface {
	EntryStats(ctx context.Context) ([]pg.QueueEntryStatsRow, error)
	LiveBatchStats(ctx context.Context) ([]pg.LiveBatchStatsRow, error)
}

// RepoLister lists managed repos. Branches of managed repos that drained keep
// reporting 0 instead of disappearing.
type RepoLister interface {
	List() []forge.RepoRef
}

// Handler serves the Prometheus exposition format for all gitea-mq metrics.
func Handler(stats StatsSource, repos RepoLister) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		enqueued, dequeued, mergedDuration, batchBuilds,
		&stateCollector{stats: stats, repos: repos, seen: map[branchRef]struct{}{}},
	)
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

var branchLabels = slices.Concat(repoLabels, []string{"branch"})

var (
	reposDesc = prometheus.NewDesc(namespace+"_managed_repos",
		"Repositories currently managed.", nil, nil)
	entriesDesc = prometheus.NewDesc(namespace+"_queue_entries",
		"Queue entries by target branch and state.", slices.Concat(branchLabels, []string{"state"}), nil)
	oldestDesc = prometheus.NewDesc(namespace+"_queue_oldest_entry_timestamp_seconds",
		"Enqueue time of the oldest entry still queued for the target branch.", branchLabels, nil)
	batchesDesc = prometheus.NewDesc(namespace+"_live_batches",
		"Live batches by target branch and state.", slices.Concat(branchLabels, []string{"state"}), nil)
)

var (
	entryStates = []pg.EntryState{pg.EntryStateQueued, pg.EntryStateTesting, pg.EntryStateSuccess}
	batchStates = []pg.BatchState{pg.BatchStateForming, pg.BatchStateTesting}
)

// scrapeTimeout bounds the database queries of one scrape.
const scrapeTimeout = 10 * time.Second

type branchRef struct {
	repo   forge.RepoRef
	branch string
}

type stateCollector struct {
	stats StatsSource
	repos RepoLister

	mu sync.Mutex
	// seen holds every target branch observed since start, so a drained
	// branch reports 0 rather than vanishing. Pruned when its repo is
	// no longer managed.
	seen map[branchRef]struct{}
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- reposDesc
	ch <- entriesDesc
	ch <- oldestDesc
	ch <- batchesDesc
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	managed := c.repos.List()
	ch <- prometheus.MustNewConstMetric(reposDesc, prometheus.GaugeValue, float64(len(managed)))

	entryRows, err := c.stats.EntryStats(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(entriesDesc, err)
		return
	}
	batchRows, err := c.stats.LiveBatchStats(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(batchesDesc, err)
		return
	}

	type key struct {
		branchRef
		state string
	}
	entries := map[key]int64{}
	batches := map[key]int64{}
	oldest := map[branchRef]time.Time{}
	current := map[branchRef]struct{}{}

	for _, row := range entryRows {
		br := branchRef{forge.RepoRef{Forge: forge.Kind(row.Forge), Owner: row.Owner, Name: row.Name}, row.TargetBranch}
		current[br] = struct{}{}
		entries[key{br, string(row.State)}] += row.Entries
		if t := row.OldestEnqueuedAt; t.Valid {
			if cur, ok := oldest[br]; !ok || t.Time.Before(cur) {
				oldest[br] = t.Time
			}
		}
	}
	for _, row := range batchRows {
		br := branchRef{forge.RepoRef{Forge: forge.Kind(row.Forge), Owner: row.Owner, Name: row.Name}, row.TargetBranch}
		current[br] = struct{}{}
		batches[key{br, string(row.State)}] += row.Batches
	}

	isManaged := make(map[forge.RepoRef]bool, len(managed))
	for _, r := range managed {
		isManaged[r] = true
	}
	c.mu.Lock()
	for br := range c.seen {
		if !isManaged[br.repo] {
			delete(c.seen, br)
		}
	}
	for br := range current {
		if isManaged[br.repo] {
			c.seen[br] = struct{}{}
		}
	}
	for br := range c.seen {
		current[br] = struct{}{}
	}
	c.mu.Unlock()

	for br := range current {
		labels := []string{string(br.repo.Forge), br.repo.Owner, br.repo.Name, br.branch}
		for _, s := range entryStates {
			ch <- prometheus.MustNewConstMetric(entriesDesc, prometheus.GaugeValue,
				float64(entries[key{br, string(s)}]), append(labels, string(s))...)
		}
		for _, s := range batchStates {
			ch <- prometheus.MustNewConstMetric(batchesDesc, prometheus.GaugeValue,
				float64(batches[key{br, string(s)}]), append(labels, string(s))...)
		}
		if t, ok := oldest[br]; ok {
			ch <- prometheus.MustNewConstMetric(oldestDesc, prometheus.GaugeValue,
				float64(t.UnixNano())/1e9, labels...)
		}
	}
}
