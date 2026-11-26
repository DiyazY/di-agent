CREATE TABLE runs (run_id text PRIMARY KEY);
CREATE TABLE run_metadata (
    run_id text NOT NULL REFERENCES runs, parameter text NOT NULL, value text,
    PRIMARY KEY (run_id, parameter)
);
CREATE TABLE timeseries (
    run_id text NOT NULL REFERENCES runs, source text NOT NULL,
    signal text NOT NULL, t double precision NOT NULL, value double precision
);
INSERT INTO runs VALUES ('ci-replay');
INSERT INTO run_metadata VALUES
    ('ci-replay', 'signals', '{"ci_replay":{"unit":"rpm","range":[0,100]}}');
INSERT INTO timeseries VALUES
    ('ci-replay', 'short', 'ci_replay', 0, 42),
    ('ci-replay', 'short', 'ci_replay', 1, 58);
