-- Deterministic fixture. Loaded into a fresh cluster by scripts/devdb.sh.
CREATE DATABASE fixture_a;
CREATE DATABASE fixture_b;

\connect fixture_a

CREATE SCHEMA app;

CREATE TABLE app.users (
  id         bigint PRIMARY KEY,
  email      text NOT NULL,
  nickname   text,
  balance    numeric(38,10),
  big        bigint,
  active     boolean,
  created_at timestamptz,
  prefs      jsonb,
  avatar     bytea
);
INSERT INTO app.users VALUES
  (1, 'ada@example.com',   NULL, 12345678901234567890.1234567890, 9223372036854775807, true,  '2026-01-02 03:04:05.123456+00', '{"theme":"dark","n":12345678901234567890}', '\xdeadbeef'),
  (2, 'bob@example.com',   '',   0,                                 -9223372036854775808, false, '1999-12-31 23:59:59+00',        '[]',                                         ''),
  (3, 'cyd@example.com',   'cy', -0.0000000001,                     0,                    NULL,  NULL,                            'null',                                       NULL),
  (4, 'dee@example.com',   'dee 100% _real_', NULL,                 42,                   true,  'infinity',                      '{"quote":"he said \"hi\""}',               '\x00');

-- 1M rows, no secondary indexes.
CREATE TABLE app.events (
  id      bigint PRIMARY KEY,
  user_id bigint,
  kind    text,
  payload jsonb,
  at      timestamptz
);
INSERT INTO app.events
SELECT g, (g % 4) + 1,
       (ARRAY['login','logout','purchase','error'])[(g % 4) + 1],
       jsonb_build_object('seq', g),
       '2026-01-01'::timestamptz + (g || ' seconds')::interval
FROM generate_series(1, 1000000) g;

-- No primary key.
CREATE TABLE public.logs (msg text, level int);
INSERT INTO public.logs VALUES ('boot', 1), ('boot', 1), ('warn', 2), (NULL, NULL);

-- Large cell for truncation.
CREATE TABLE public.blobs (id int PRIMARY KEY, body text);
INSERT INTO public.blobs VALUES (1, repeat('x', 50000));

CREATE VIEW app.active_users AS SELECT id, email FROM app.users WHERE active;

-- Structure view: an index, a foreign key (events -> users) and a default.
CREATE INDEX events_user_kind_idx ON app.events (user_id, kind);
ALTER TABLE app.events ADD CONSTRAINT events_user_fk FOREIGN KEY (user_id) REFERENCES app.users (id);
ALTER TABLE app.users ALTER COLUMN active SET DEFAULT true;

ANALYZE;

\connect fixture_b
CREATE TABLE public.orders (id int PRIMARY KEY, item text, qty int);
INSERT INTO public.orders VALUES (1, 'apple', 3), (2, 'pear', 0), (3, 'fig', NULL);
ANALYZE;
