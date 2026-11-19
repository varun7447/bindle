-- More alert rule kinds: negative user feedback, cost regression between
-- agent versions, new error patterns, and the weekly digest.
ALTER TABLE control.alert_rules DROP CONSTRAINT alert_rules_kind_check;
ALTER TABLE control.alert_rules ADD CONSTRAINT alert_rules_kind_check
    CHECK (kind IN ('error_rate', 'runaway_loop', 'run_cost', 'negative_feedback', 'cost_regression', 'new_error', 'weekly_digest'));
