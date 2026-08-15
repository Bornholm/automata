package observability

import (
	"sync"
	"testing"
	"time"
)

func TestMetrics_NilSafe(t *testing.T) {
	var m *Metrics

	// Aucune de ces méthodes ne doit paniquer sur un récepteur nil.
	m.IncMessagesReceived()
	m.IncMessagesIgnoredNoMention()
	m.IncUnknownOrigin()
	m.IncDuplicateMessage()
	m.ObserveTranscriptionLatency(time.Second)
	m.ObserveAgentLatency(time.Second)
	m.IncDelegation("agenda")
	m.IncMCPCall("server", "tool", nil)
	m.IncActionProposed()
	m.IncActionConfirmed()
	m.IncMemorySearch()
	m.IncCronOccurrence("daily-report")
	m.IncDeliveryError()
	m.IncToolResultTruncated()

	if snap := m.Snapshot(); len(snap) != 0 {
		t.Fatalf("Snapshot() sur un registre nil doit être vide, obtenu: %v", snap)
	}
}

func TestMetrics_Counters(t *testing.T) {
	m := NewMetrics()

	m.IncMessagesReceived()
	m.IncMessagesReceived()
	m.IncMessagesIgnoredNoMention()
	m.IncUnknownOrigin()
	m.IncDuplicateMessage()
	m.IncActionProposed()
	m.IncActionProposed()
	m.IncActionConfirmed()
	m.IncMemorySearch()
	m.IncDeliveryError()
	m.IncToolResultTruncated()

	snap := m.Snapshot()

	cases := map[string]int64{
		"messages_received":           2,
		"messages_ignored_no_mention": 1,
		"unknown_origin":              1,
		"duplicate_messages":          1,
		"actions_proposed":            2,
		"actions_confirmed":           1,
		"memory_searches":             1,
		"delivery_errors":             1,
		"tool_results_truncated":      1,
	}

	for key, want := range cases {
		got, ok := snap[key].(int64)
		if !ok {
			t.Fatalf("Snapshot()[%q] type inattendu: %T (%v)", key, snap[key], snap[key])
		}
		if got != want {
			t.Errorf("Snapshot()[%q] = %d, attendu %d", key, got, want)
		}
	}
}

func TestMetrics_DelegationsByAgent(t *testing.T) {
	m := NewMetrics()

	m.IncDelegation("agenda")
	m.IncDelegation("agenda")
	m.IncDelegation("todo")

	snap := m.Snapshot()
	delegations, ok := snap["delegations_by_agent"].(map[string]int64)
	if !ok {
		t.Fatalf("delegations_by_agent type inattendu: %T", snap["delegations_by_agent"])
	}

	if delegations["agenda"] != 2 {
		t.Errorf("delegations[agenda] = %d, attendu 2", delegations["agenda"])
	}
	if delegations["todo"] != 1 {
		t.Errorf("delegations[todo] = %d, attendu 1", delegations["todo"])
	}
}

func TestMetrics_MCPCalls(t *testing.T) {
	m := NewMetrics()

	m.IncMCPCall("google-calendar", "list_events", nil)
	m.IncMCPCall("google-calendar", "list_events", nil)
	m.IncMCPCall("google-calendar", "list_events", errTest)

	snap := m.Snapshot()
	byServer, ok := snap["mcp_calls"].(map[string]map[string]int64)
	if !ok {
		t.Fatalf("mcp_calls type inattendu: %T", snap["mcp_calls"])
	}

	stats := byServer["google-calendar"]
	if stats["list_events_success"] != 2 {
		t.Errorf("list_events_success = %d, attendu 2", stats["list_events_success"])
	}
	if stats["list_events_error"] != 1 {
		t.Errorf("list_events_error = %d, attendu 1", stats["list_events_error"])
	}
}

func TestMetrics_CronOccurrences(t *testing.T) {
	m := NewMetrics()

	m.IncCronOccurrence("daily-report")
	m.IncCronOccurrence("daily-report")
	m.IncCronOccurrence("weekly-digest")

	snap := m.Snapshot()
	occurrences, ok := snap["cron_occurrences"].(map[string]int64)
	if !ok {
		t.Fatalf("cron_occurrences type inattendu: %T", snap["cron_occurrences"])
	}

	if occurrences["daily-report"] != 2 {
		t.Errorf("daily-report = %d, attendu 2", occurrences["daily-report"])
	}
	if occurrences["weekly-digest"] != 1 {
		t.Errorf("weekly-digest = %d, attendu 1", occurrences["weekly-digest"])
	}
}

func TestMetrics_LatencyAggregates(t *testing.T) {
	m := NewMetrics()

	m.ObserveAgentLatency(100 * time.Millisecond)
	m.ObserveAgentLatency(300 * time.Millisecond)
	m.ObserveAgentLatency(200 * time.Millisecond)

	snap := m.Snapshot()
	agentLatency, ok := snap["agent_latency"].(latencySnapshot)
	if !ok {
		t.Fatalf("agent_latency type inattendu: %T", snap["agent_latency"])
	}

	if agentLatency.Count != 3 {
		t.Errorf("count = %d, attendu 3", agentLatency.Count)
	}
	if agentLatency.MinMS != 100 {
		t.Errorf("min_ms = %v, attendu 100", agentLatency.MinMS)
	}
	if agentLatency.MaxMS != 300 {
		t.Errorf("max_ms = %v, attendu 300", agentLatency.MaxMS)
	}
	if agentLatency.AvgMS != 200 {
		t.Errorf("avg_ms = %v, attendu 200", agentLatency.AvgMS)
	}
}

func TestMetrics_ConcurrentAccess(t *testing.T) {
	m := NewMetrics()

	var wg sync.WaitGroup
	const goroutines = 50
	const perGoroutine = 100

	for i := range goroutines {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for range perGoroutine {
				m.IncMessagesReceived()
				m.IncDelegation("agenda")
				m.IncMCPCall("server", "tool", nil)
				m.IncCronOccurrence("daily-report")
				m.ObserveAgentLatency(time.Duration(n) * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()

	snap := m.Snapshot()
	if got := snap["messages_received"].(int64); got != goroutines*perGoroutine {
		t.Errorf("messages_received = %d, attendu %d", got, goroutines*perGoroutine)
	}

	delegations := snap["delegations_by_agent"].(map[string]int64)
	if delegations["agenda"] != goroutines*perGoroutine {
		t.Errorf("delegations[agenda] = %d, attendu %d", delegations["agenda"], goroutines*perGoroutine)
	}
}

var errTest = &testError{"erreur de test"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
