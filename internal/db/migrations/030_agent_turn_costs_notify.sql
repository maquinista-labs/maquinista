-- 030_agent_turn_costs_notify.sql
--
-- C.1 of plans/active/dashboard-cost-sse.md: fire pg_notify on every
-- new agent_turn_costs row so the dashboard can invalidate the KPI
-- strip without polling.
--
-- Payload is the agent_id so clients can scope partial invalidations
-- in the future (today they invalidate the full ["kpis"] query key).

CREATE OR REPLACE FUNCTION notify_agent_turn_cost()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('agent_turn_cost_new', NEW.agent_id);
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS on_agent_turn_cost_notify ON agent_turn_costs;
CREATE TRIGGER on_agent_turn_cost_notify
    AFTER INSERT ON agent_turn_costs
    FOR EACH ROW EXECUTE FUNCTION notify_agent_turn_cost();
