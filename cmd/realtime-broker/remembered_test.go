package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/agentmemory"
	"github.com/JeremyProffittOrg/live-ninja/internal/agentmemory/agentmemorytest"
	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
)

// agentcore-memory (plan.md mint-preload): the REMEMBERED block is rendered
// from one bounded retrieval and is absent — never an error — when memory is
// off, the role is not admitted, retrieval fails, or the budget runs out.

func rememberedBroker(fake *agentmemorytest.Fake, mode agentmemory.Mode) *broker {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &broker{log: log, memory: agentmemory.New(fake, agentmemory.Config{MemoryID: "mem-1", Mode: mode}, log)}
}

func TestRememberedBlockRendersTopRecords(t *testing.T) {
	fake := &agentmemorytest.Fake{Records: []types.MemoryRecordSummary{
		agentmemorytest.Summary("r1", "/users/u1/facts/", "Drives a 2019 Outback", 0.8),
		agentmemorytest.Summary("r2", "/users/u1/preferences/", "Prefers Celsius", 0.7),
	}}
	b := rememberedBroker(fake, agentmemory.ModeOwner)

	block := b.rememberedBlock(context.Background(), b.log, Request{UserID: "u1", Role: "owner"})
	require.Contains(t, block, realtime.RememberedHeader)
	assert.Contains(t, block, "- Drives a 2019 Outback")
	assert.Contains(t, block, "- Prefers Celsius")
	require.Len(t, fake.Retrieves, 1)
	assert.Equal(t, "/users/u1/", *fake.Retrieves[0].NamespacePath)
}

func TestRememberedBlockIsEmptyOutsideTheRolloutMode(t *testing.T) {
	fake := &agentmemorytest.Fake{Records: []types.MemoryRecordSummary{agentmemorytest.Summary("r1", "/users/u1/facts/", "x", 0.8)}}
	b := rememberedBroker(fake, agentmemory.ModeOwner)
	assert.Equal(t, "", b.rememberedBlock(context.Background(), b.log, Request{UserID: "u1", Role: "member"}))
	assert.Empty(t, fake.Retrieves, "no retrieval is billed for a caller the mode does not admit")

	var nilBroker = &broker{log: b.log}
	assert.Equal(t, "", nilBroker.rememberedBlock(context.Background(), b.log, Request{UserID: "u1", Role: "owner"}),
		"memory not configured mints exactly as before")
}

func TestRememberedBlockGivesUpAtTheBudget(t *testing.T) {
	fake := &agentmemorytest.Fake{
		Records:       []types.MemoryRecordSummary{agentmemorytest.Summary("r1", "/users/u1/facts/", "slow", 0.8)},
		RetrieveDelay: 3 * time.Second,
	}
	b := rememberedBroker(fake, agentmemory.ModeAll)
	start := time.Now()
	block := b.rememberedBlock(context.Background(), b.log, Request{UserID: "u1", Role: "member"})
	assert.Equal(t, "", block)
	assert.Less(t, time.Since(start), 2*time.Second, "the mint must not wait for a slow AgentCore")
}

func TestRememberedBlockSwallowsRetrievalErrors(t *testing.T) {
	fake := &agentmemorytest.Fake{RetrieveErr: errors.New("throttled")}
	b := rememberedBroker(fake, agentmemory.ModeAll)
	assert.Equal(t, "", b.rememberedBlock(context.Background(), b.log, Request{UserID: "u1", Role: "member"}))
}
