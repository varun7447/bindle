-- Alert rule kind for passport drift (an agent going outside its passport:
-- unlisted tools, data classes, blocked egress, release actions, rate
-- limits, budgets). One constraint lists every kind.
ALTER TABLE control.alert_rules DROP CONSTRAINT alert_rules_kind_check;
ALTER TABLE control.alert_rules ADD CONSTRAINT alert_rules_kind_check
    CHECK (kind IN ('error_rate', 'runaway_loop', 'run_cost', 'negative_feedback', 'cost_regression', 'new_error', 'weekly_digest',
                    'security_signal', 'passport_drift'));

-- Drift found by the alerts worker (rate limits, budgets) is written to the
-- agent's ledger, through the same append functions the pipeline uses.
GRANT EXECUTE ON FUNCTION control.ledger_lock_head(text, text), control.ledger_known(text, text[], text[], text[], text[]),
    control.ledger_append(text, text, bigint, jsonb) TO {{alerts}};
