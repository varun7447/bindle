-- Where each dataset item came from: "" when added by hand from a run, or a
-- short label such as "failure: timeout after <n>s" or "negative feedback"
-- when added in bulk from production problems.
ALTER TABLE control.dataset_items ADD COLUMN source text NOT NULL DEFAULT '';
