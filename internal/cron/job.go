// Package cron implements MTClaw's in-process scheduler: YAML-declared
// prompts run on a cron expression and their result is delivered to a chat,
// reusing the gateway dispatcher for serialization and delivery.
// Scheduler.tryFire is the core of it: two independent guards (minute
// de-dup, so a ticker firing twice for the same due minute cannot double
// its turn; overlap, so a slow-running turn cannot get a second one
// stacked on top of it) protect one fire.
package cron

import (
	"time"

	"github.com/tiennm99/MTClaw/internal/channel"
	"github.com/tiennm99/MTClaw/internal/config"
)

// Job is one scheduled prompt, resolved from config.CronJob into the shape
// the scheduler acts on. config.Validate has already rejected malformed
// entries (duplicate names, invalid schedules, unreachable delivery
// targets) by the time JobsFromConfig runs, so this type carries no
// validation of its own.
type Job struct {
	Name      string
	Schedule  string
	Prompt    string
	Enabled   bool
	Ephemeral bool // false = persistent: session:"ephemeral" in YAML
	Timeout   time.Duration
	DeliverTo channel.DeliverTarget
}

// chatID is the fixed session identity every run of this job shares
// (persistent jobs) or is keyed under alongside a per-run thread id
// (ephemeral jobs) - see Scheduler.fire.
func (j Job) chatID() string { return "job:" + j.Name }

// JobsFromConfig converts every configured cron job into a Job, called once
// at Scheduler construction time.
func JobsFromConfig(cfg config.CronConfig) []Job {
	jobs := make([]Job, 0, len(cfg.Jobs))
	for _, j := range cfg.Jobs {
		jobs = append(jobs, Job{
			Name:      j.Name,
			Schedule:  j.Schedule,
			Prompt:    j.Prompt,
			Enabled:   j.Enabled,
			Ephemeral: j.Session == "ephemeral",
			Timeout:   j.Timeout.Std(),
			DeliverTo: channel.DeliverTarget{ChatID: j.DeliverTo.ChatID},
		})
	}
	return jobs
}
