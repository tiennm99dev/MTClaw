package cron

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tiennm99/MTClaw/internal/config"
)

func TestJobsFromConfig_ConvertsEveryField(t *testing.T) {
	cfg := config.CronConfig{
		Jobs: []config.CronJob{
			{
				Name:     "persistent-job",
				Schedule: "0 8 * * *",
				Prompt:   "summarize",
				Enabled:  true,
				Session:  "persistent",
				Timeout:  config.Duration(90 * time.Second),
				DeliverTo: config.CronDeliverTo{
					Channel: "telegram",
					ChatID:  "123",
				},
			},
			{
				Name:     "ephemeral-job",
				Schedule: "*/5 * * * *",
				Prompt:   "check something",
				Enabled:  false,
				Session:  "ephemeral",
				Timeout:  config.Duration(30 * time.Second),
				DeliverTo: config.CronDeliverTo{
					Channel: "telegram",
					ChatID:  "456",
				},
			},
		},
	}

	jobs := JobsFromConfig(cfg)
	assert.Len(t, jobs, 2)

	p := jobs[0]
	assert.Equal(t, "persistent-job", p.Name)
	assert.Equal(t, "0 8 * * *", p.Schedule)
	assert.Equal(t, "summarize", p.Prompt)
	assert.True(t, p.Enabled)
	assert.False(t, p.Ephemeral)
	assert.Equal(t, 90*time.Second, p.Timeout)
	assert.Equal(t, "telegram", p.DeliverTo.Channel)
	assert.Equal(t, "123", p.DeliverTo.ChatID)
	assert.Equal(t, "job:persistent-job", p.chatID())

	e := jobs[1]
	assert.False(t, e.Enabled)
	assert.True(t, e.Ephemeral)
	assert.Equal(t, "job:ephemeral-job", e.chatID())
}
