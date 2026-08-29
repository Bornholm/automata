package config

import "testing"

func TestValidateMemory_UnknownIndexTypeIsRejected(t *testing.T) {
	cfg := &Config{
		Memory: Memory{
			Indexes: []MemoryIndex{{ID: "idx", Type: "pinecone", Path: "x"}},
		},
	}
	assertHasError(t, validateMemory(cfg), `memory.indexes[0].type: "pinecone" non supporté`)
}
