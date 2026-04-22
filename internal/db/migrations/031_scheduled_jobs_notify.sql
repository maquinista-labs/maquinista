-- 031_scheduled_jobs_notify.sql
--
-- C.2 of plans/active/dashboard-cost-sse.md: fire pg_notify on
-- INSERT / UPDATE / DELETE against scheduled_jobs and webhook_handlers
-- so the dashboard jobs page refreshes immediately rather than
-- waiting for the 60 s polling interval.
--
-- Payload is the row id (text-cast UUID/bigint) so clients can
-- target a single card in the future.

CREATE OR REPLACE FUNCTION notify_scheduled_jobs_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('scheduled_jobs_change',
        COALESCE(NEW.id::text, OLD.id::text));
    RETURN COALESCE(NEW, OLD);
END;
$$;

DROP TRIGGER IF EXISTS on_scheduled_jobs_notify ON scheduled_jobs;
CREATE TRIGGER on_scheduled_jobs_notify
    AFTER INSERT OR UPDATE OR DELETE ON scheduled_jobs
    FOR EACH ROW EXECUTE FUNCTION notify_scheduled_jobs_change();

CREATE OR REPLACE FUNCTION notify_webhook_handlers_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('webhook_handlers_change',
        COALESCE(NEW.id::text, OLD.id::text));
    RETURN COALESCE(NEW, OLD);
END;
$$;

DROP TRIGGER IF EXISTS on_webhook_handlers_notify ON webhook_handlers;
CREATE TRIGGER on_webhook_handlers_notify
    AFTER INSERT OR UPDATE OR DELETE ON webhook_handlers
    FOR EACH ROW EXECUTE FUNCTION notify_webhook_handlers_change();
