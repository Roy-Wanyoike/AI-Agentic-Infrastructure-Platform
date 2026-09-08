-- 024_notifications_subscriptions.sql
--
-- Issue #83: operational alert subscriptions (internal/notifications).
--
-- A notification subscription is an org-scoped routing rule binding ONE
-- existing webhook endpoint (migration 010, same organization) to a set of
-- event types from the pinned contract (internal/events). At dispatch time the
-- notifications subscriber resolves the subscription's webhook through the
-- webhooks service, so deliveries reuse the standard signed delivery pipeline
-- and surface in webhook_deliveries — no new delivery state is stored here.
--
--   * notification_subscriptions: id (TEXT PK, UUID), organization_id (tenant
--     guard, FK CASCADE), webhook_id (FK CASCADE — deleting the webhook makes
--     the subscription meaningless, so it follows), event_types (JSONB array
--     of contract event types, mirrors the webhooks.events column style),
--     created_at.
--
-- All statements are idempotent (IF NOT EXISTS) per the 001-021 pattern so
-- cmd/migrate can be re-run safely. Event-type validation (against
-- events.AllEventTypes) and same-organization webhook validation live in the
-- service layer, not in SQL CHECKs, so new pinned contract types need no
-- migration.

CREATE TABLE IF NOT EXISTS notification_subscriptions (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    webhook_id TEXT NOT NULL,
    event_types JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_notification_subscriptions_org FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT fk_notification_subscriptions_webhook FOREIGN KEY (webhook_id) REFERENCES webhooks(id) ON DELETE CASCADE
);

-- Tenant-scoped indexes (organization_id + created_at hot path, plus the
-- per-webhook lookup used when a webhook is deleted).
CREATE INDEX IF NOT EXISTS idx_notification_subscriptions_org_created ON notification_subscriptions(organization_id, created_at);
CREATE INDEX IF NOT EXISTS idx_notification_subscriptions_webhook ON notification_subscriptions(webhook_id);
