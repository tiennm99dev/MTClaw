package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tiennm99/MTClaw/internal/agent"
	"github.com/tiennm99/MTClaw/internal/channel"
	"github.com/tiennm99/MTClaw/internal/channel/telegram"
	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/cron"
	"github.com/tiennm99/MTClaw/internal/provider/openai"
	"github.com/tiennm99/MTClaw/internal/store"
	"github.com/tiennm99/MTClaw/internal/store/sqlite"
	"github.com/tiennm99/MTClaw/internal/tools"
)

// Gateway is the fully-wired, long-running mtclaw process: store, provider,
// tool registry, Telegram channel, and dispatcher. New performs every step
// of the startup order up to (but not including) starting the channel's
// long-poll loop; Run starts that loop plus the dispatcher and signal
// watcher, and blocks until shutdown completes.
type Gateway struct {
	log       *slog.Logger
	store     store.Store
	channel   *telegram.Channel
	disp      *dispatcher
	cronSched *cron.Scheduler // nil when cron.enabled is false

	release func() error
}

// New wires a Gateway in startup order: instance lock, store (with
// migrations), OpenAI provider, tool registry (with the approver mux),
// Telegram channel, then the dispatcher. Any failure here leaves nothing
// partially running: whatever was already opened is torn down before
// returning the error.
func New(cfg config.Config, log *slog.Logger) (*Gateway, error) {
	if log == nil {
		log = slog.Default()
	}
	if !cfg.Channels.Telegram.Enabled {
		return nil, fmt.Errorf("gateway: channels.telegram.enabled must be true to run the gateway")
	}

	stateDir, err := config.StateDir()
	if err != nil {
		return nil, fmt.Errorf("gateway: resolve state directory: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("gateway: create state directory %s: %w", stateDir, err)
	}
	lockPath := filepath.Join(stateDir, lockFileName)
	release, err := Acquire(lockPath)
	if err != nil {
		// Acquire's own error is already a well-formed "gateway: ..."
		// message (it names the blocking pid), so it is returned as-is
		// rather than wrapped again into "gateway: gateway: ...".
		return nil, err
	}

	db, err := sqlite.Open(context.Background(), cfg.Storage.Path, false)
	if err != nil {
		release()
		return nil, fmt.Errorf("gateway: open store: %w", err)
	}
	st := sqlite.New(db)

	// A cron_runs row still "started" from a prior process (a SIGKILL, an
	// OOM, a dropped dispatch - anything that skipped OnDone) would
	// otherwise report as perpetually in flight forever; sweep it now,
	// mirroring the Telegram approver's own ExpirePending sweep at its
	// Start. Runs unconditionally, not just when cron.enabled, since a
	// disabled job can still have a stale row from when it was enabled.
	// Bounded, not context.Background(): a held write lock (a manual
	// `mtclaw cron run`, a backup holding the database's write lock) must
	// not hang startup indefinitely.
	expireCtx, expireCancel := context.WithTimeout(context.Background(), 10*time.Second)
	n, err := st.CronRuns().ExpireStarted(expireCtx, time.Now())
	expireCancel()
	if err != nil {
		log.Error("gateway: expire stale cron runs at startup failed", "error", err)
	} else if n > 0 {
		log.Info("gateway: expired cron runs left running by a prior process", "count", n)
	}

	client, err := openai.New(cfg.OpenAI)
	if err != nil {
		st.Close()
		release()
		return nil, fmt.Errorf("gateway: build openai client: %w", err)
	}

	mux := newApproverMux()
	registry, err := tools.New(cfg, st, mux, log)
	if err != nil {
		st.Close()
		release()
		return nil, fmt.Errorf("gateway: build tool registry: %w", err)
	}

	// The dispatcher must exist before the Telegram channel does, because
	// the channel's Deps (for /stop) needs a cancel registry to call into -
	// but the dispatcher's own channel/runner fields are only filled in
	// below, once those exist. Nothing reads them before Run starts the
	// pump, so this ordering is safe.
	disp := newDispatcher(st, nil, nil, log, idleSessionTimeout, globalConcurrency)

	deps := &telegramDeps{cfg: cfg, store: st, disp: disp}
	tgChannel, err := telegram.New(&cfg, st.Approvals(), deps, log)
	if err != nil {
		st.Close()
		release()
		return nil, fmt.Errorf("gateway: build telegram channel: %w", err)
	}
	mux.setTelegram(tgChannel.Approver())

	loop := agent.New(cfg, client, st, registry, log)

	disp.channel = tgChannel
	disp.runner = loop

	var sched *cron.Scheduler
	if cfg.Cron.Enabled {
		loc, err := time.LoadLocation(cfg.Cron.Timezone)
		if err != nil {
			// config.Validate already rejects an unloadable timezone before
			// a Config value can reach here; this is a defensive
			// last-resort, not the primary check.
			st.Close()
			release()
			return nil, fmt.Errorf("gateway: cron.timezone: %w", err)
		}
		sched = cron.New(cron.JobsFromConfig(cfg.Cron), loc, st.CronRuns(), st.Sessions(), disp.dispatch, log, nil)
	}

	return &Gateway{
		log:       log,
		store:     st,
		channel:   tgChannel,
		disp:      disp,
		cronSched: sched,
		release:   release,
	}, nil
}

// Run starts the channel's long-poll loop, the global-inbound relay, the
// dispatcher, and a SIGINT/SIGTERM watcher, then blocks until shutdown
// completes. Shutdown order is the reverse of startup: the channel's update
// pump stops first (no new inbound arrives), in-flight turns drain (bounded
// by drainDeadline), then the store closes and the lock releases - closing
// the store while a worker is mid-Append would lose a turn. If the drain
// deadline is breached, the store is deliberately left open (closing a
// database out from under a still-writing worker is worse than leaking the
// fd at process exit) and Run returns a non-nil error so the process exits
// non-zero instead of looking like a clean shutdown.
func (g *Gateway) Run(ctx context.Context) error {
	var drained bool
	defer func() {
		if drained {
			if err := g.store.Close(); err != nil {
				g.log.Error("gateway: close store failed", "error", err)
			}
		} else {
			g.log.Error("gateway: drain deadline exceeded; leaving the store open for process exit to close instead of closing it under a possibly still-writing worker")
		}
		if err := g.release(); err != nil {
			g.log.Error("gateway: release instance lock failed", "error", err)
		}
	}()

	// A plain cancelable child of ctx, not a second signal.NotifyContext:
	// the cli root already installs one (internal/cli/root.go) to catch
	// SIGINT/SIGTERM, and a second registered handler here would steal the
	// second, hard-kill Ctrl-C the cli's own handler is there to honor.
	// runCtx still ends whenever ctx does (a real signal, or a caller-driven
	// shutdown in tests); stop is additionally used below to end it early
	// when the channel itself fails.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	g.disp.rootCtx = runCtx

	// Mark the dispatcher closed as soon as runCtx ends, before
	// waitForShutdown's own wg.Wait() can itself return - see dispatch's
	// d.closed field and closeForShutdown for the wg.Add/wg.Wait race this
	// closes.
	go func() {
		<-runCtx.Done()
		g.disp.closeForShutdown()
	}()

	raw := make(chan channel.Inbound)
	global := make(chan channel.Inbound, globalQueueSize)

	var pumps sync.WaitGroup
	pumps.Add(3)
	if g.cronSched != nil {
		pumps.Add(1)
	}

	// channelFailure is written only by the channel goroutine below, and
	// only before it calls stop() (which is what waitForShutdown blocks
	// on); pumps.Wait() below happens-after that write via the WaitGroup,
	// so reading it after both calls return is race-free.
	var channelFailure error
	go func() {
		defer pumps.Done()
		err := g.channel.Start(runCtx, raw)
		if err != nil && runCtx.Err() == nil {
			// The channel is the gateway's only inbound surface: if its
			// poll loop dies for a reason other than shutdown (a revoked
			// token, a sustained network failure), there is no path left
			// for new messages to ever arrive. Treat it as fatal - cancel
			// everything else the same way a signal would, and surface the
			// error so the process exits non-zero instead of idling
			// forever looking alive.
			channelFailure = fmt.Errorf("gateway: channel stopped unexpectedly: %w", err)
			g.log.Error("gateway: channel stopped unexpectedly; shutting down", "error", err)
			stop()
		}
	}()
	go func() {
		defer pumps.Done()
		relayInbound(runCtx, raw, global, g.channel, g.log)
	}()
	go func() {
		defer pumps.Done()
		g.disp.pump(runCtx, global)
	}()
	if g.cronSched != nil {
		go func() {
			defer pumps.Done()
			g.cronSched.Start(runCtx)
		}()
	}

	drained = waitForShutdown(runCtx, &g.disp.wg, g.log)
	pumps.Wait()
	if channelFailure != nil {
		return channelFailure
	}
	if !drained {
		return fmt.Errorf("gateway: shutdown drain deadline exceeded; some turns may still be running")
	}
	return nil
}

// telegramDeps implements telegram.Deps against a real store and the
// gateway's dispatcher, so /new, /status, and /stop act on the same session
// and cancel registry the dispatcher itself uses.
type telegramDeps struct {
	cfg   config.Config
	store store.Store
	disp  *dispatcher
}

var _ telegram.Deps = (*telegramDeps)(nil)

func (d *telegramDeps) Status(ctx context.Context, chatID, threadID string) (telegram.SessionStatus, error) {
	sess, err := d.store.Sessions().Ensure(ctx, "telegram", chatID, threadID)
	if err != nil {
		return telegram.SessionStatus{}, err
	}
	count, err := d.store.Messages().CountBySession(ctx, sess.ID)
	if err != nil {
		return telegram.SessionStatus{}, err
	}
	return telegram.SessionStatus{
		SessionID:        sess.ID,
		Messages:         count,
		PromptTokens:     sess.PromptTokens,
		CompletionTokens: sess.CompletionTokens,
		Model:            d.cfg.Agent.Model,
		CreatedAt:        sess.CreatedAt,
	}, nil
}

func (d *telegramDeps) Reset(ctx context.Context, chatID, threadID string) error {
	sess, err := d.store.Sessions().Ensure(ctx, "telegram", chatID, threadID)
	if err != nil {
		return err
	}
	return d.store.Messages().DeleteBySession(ctx, sess.ID)
}

func (d *telegramDeps) Cancel(chatID, threadID string) bool {
	return d.disp.cancelSession(sessionKey("telegram", chatID, threadID))
}
