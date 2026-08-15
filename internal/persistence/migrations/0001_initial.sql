CREATE TABLE principals (
    id TEXT PRIMARY KEY,
    org_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    display_name TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE conversations (
    id TEXT PRIMARY KEY,
    org_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    external_channel_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    scope TEXT NOT NULL,
    scope_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (provider, external_channel_id)
);

CREATE TABLE messages (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations (id),
    external_message_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    role TEXT NOT NULL,
    content TEXT NOT NULL,
    content_kind TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (conversation_id, external_message_id)
);

CREATE INDEX idx_messages_conversation_id ON messages (conversation_id);

CREATE TABLE action_plans (
    id TEXT PRIMARY KEY,
    org_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL REFERENCES conversations (id),
    created_by TEXT NOT NULL,
    scope TEXT NOT NULL,
    scope_id TEXT NOT NULL,
    status TEXT NOT NULL,
    expires_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX idx_action_plans_conversation_id ON action_plans (conversation_id);

CREATE TABLE actions (
    id TEXT PRIMARY KEY,
    plan_id TEXT NOT NULL REFERENCES action_plans (id),
    position INTEGER NOT NULL,
    agent_id TEXT NOT NULL,
    mcp_server TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    arguments_json TEXT NOT NULL,
    summary TEXT NOT NULL,
    required_permission TEXT NOT NULL,
    requires_confirmation INTEGER NOT NULL,
    status TEXT NOT NULL,
    error_code TEXT,
    created_at TEXT NOT NULL,
    started_at TEXT,
    completed_at TEXT
);

CREATE INDEX idx_actions_plan_id ON actions (plan_id);

CREATE TABLE processed_messages (
    provider TEXT NOT NULL,
    external_message_id TEXT NOT NULL,
    processed_at TEXT NOT NULL,
    status TEXT NOT NULL,
    PRIMARY KEY (provider, external_message_id)
);

CREATE TABLE scheduled_runs (
    id TEXT PRIMARY KEY,
    schedule_id TEXT NOT NULL,
    scheduled_for TEXT NOT NULL,
    started_at TEXT,
    completed_at TEXT,
    status TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    org_id TEXT NOT NULL,
    scope TEXT NOT NULL,
    scope_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    error_code TEXT,
    delivery_status TEXT,
    created_at TEXT NOT NULL,
    UNIQUE (schedule_id, scheduled_for)
);

CREATE TABLE delivery_attempts (
    id TEXT PRIMARY KEY,
    scheduled_run_id TEXT NOT NULL REFERENCES scheduled_runs (id),
    provider TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    attempt INTEGER NOT NULL,
    status TEXT NOT NULL,
    error_code TEXT,
    created_at TEXT NOT NULL,
    completed_at TEXT
);

CREATE INDEX idx_delivery_attempts_scheduled_run_id ON delivery_attempts (scheduled_run_id);

CREATE TABLE audit_events (
    id TEXT PRIMARY KEY,
    org_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    trigger TEXT NOT NULL,
    conversation_id TEXT,
    event_type TEXT NOT NULL,
    resource_kind TEXT NOT NULL,
    resource_scope TEXT NOT NULL,
    resource_scope_id TEXT NOT NULL,
    outcome TEXT NOT NULL,
    metadata_json TEXT,
    created_at TEXT NOT NULL
);
