-- Alert rule kind for security signals (prompt injection, exposed
-- credentials, possible exfiltration) on agent steps.
ALTER TABLE control.alert_rules DROP CONSTRAINT alert_rules_kind_check;
ALTER TABLE control.alert_rules ADD CONSTRAINT alert_rules_kind_check
    CHECK (kind IN ('error_rate', 'runaway_loop', 'run_cost', 'negative_feedback', 'cost_regression', 'new_error', 'weekly_digest', 'security_signal'));
