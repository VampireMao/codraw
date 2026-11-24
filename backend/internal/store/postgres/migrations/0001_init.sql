-- ============================================================
-- 0001_init.sql · CoDraw schema v1
-- 由 docs/DATA-MODEL.md 同步而来；两者必须一致。
-- ============================================================

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------- 房间 ----------
CREATE TABLE IF NOT EXISTS rooms (
    code                text        PRIMARY KEY,
    title               text        NOT NULL DEFAULT '未命名白板',
    epoch               integer     NOT NULL DEFAULT 1,
    editor_key_hash     bytea       NOT NULL,
    viewer_key_hash     bytea       NOT NULL,
    readonly_min_iat    bigint      NOT NULL DEFAULT 0,
    shape_count         integer     NOT NULL DEFAULT 0,
    last_seq            bigint      NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL DEFAULT now(),
    last_active_at      timestamptz NOT NULL DEFAULT now(),
    expires_at          timestamptz NULL,

    CONSTRAINT rooms_code_fmt CHECK (code ~ '^[2-9A-HJKMNP-Z]{6}$'),
    CONSTRAINT rooms_title_len CHECK (char_length(title) BETWEEN 1 AND 60),
    CONSTRAINT rooms_last_seq_nonneg CHECK (last_seq >= 0)
);

CREATE INDEX IF NOT EXISTS idx_rooms_expires
    ON rooms (expires_at)
    WHERE expires_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_rooms_last_active
    ON rooms (last_active_at DESC);

-- ---------- 事件日志（真相源） ----------
CREATE TABLE IF NOT EXISTS events (
    room_code   text        NOT NULL REFERENCES rooms(code) ON DELETE CASCADE,
    seq         bigint      NOT NULL,
    op_id       text        NOT NULL,
    client_id   text        NOT NULL,
    author_id   text        NOT NULL,
    action      text        NOT NULL,
    shape_id    text        NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (room_code, seq),
    CONSTRAINT events_action_enum CHECK (action IN ('create','update','delete','append','meta')),
    CONSTRAINT events_seq_pos CHECK (seq > 0)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_events_idem
    ON events (room_code, client_id, op_id);

CREATE INDEX IF NOT EXISTS idx_events_shape
    ON events (room_code, shape_id)
    WHERE shape_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_events_created
    ON events (room_code, created_at);

-- ---------- 快照（缓存，可重建） ----------
CREATE TABLE IF NOT EXISTS snapshots (
    room_code   text        NOT NULL REFERENCES rooms(code) ON DELETE CASCADE,
    seq         bigint      NOT NULL,
    epoch       integer     NOT NULL,
    shape_count integer     NOT NULL,
    codec       text        NOT NULL DEFAULT 'zstd',
    data        bytea       NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (room_code, seq),
    CONSTRAINT snapshots_codec_enum CHECK (codec IN ('zstd','gzip','none'))
);

CREATE INDEX IF NOT EXISTS idx_snapshots_latest
    ON snapshots (room_code, seq DESC);

-- ---------- 成员（可选） ----------
CREATE TABLE IF NOT EXISTS members (
    room_code    text        NOT NULL REFERENCES rooms(code) ON DELETE CASCADE,
    user_id      text        NOT NULL,
    name         text        NOT NULL,
    color        text        NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (room_code, user_id)
);

CREATE INDEX IF NOT EXISTS idx_members_seen
    ON members (room_code, last_seen_at DESC);

-- ---------- 权限模型：房主与成员授权（docs/PERMISSIONS.md §7） ----------
-- 幂等：表是新建的，所以 ADD COLUMN 放在 DO 块里判断，重复执行不报错。
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS owner_client_id text NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS room_grants (
    room_id     text   NOT NULL REFERENCES rooms(code) ON DELETE CASCADE,
    client_id   text   NOT NULL,
    role        text   NOT NULL CHECK (role IN ('editor','viewer')),
    granted_by  text   NOT NULL,
    updated_at  bigint NOT NULL,
    PRIMARY KEY (room_id, client_id)
);

CREATE INDEX IF NOT EXISTS idx_room_grants_granted_by
    ON room_grants (room_id, granted_by);
