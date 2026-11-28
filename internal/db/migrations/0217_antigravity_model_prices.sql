-- Seed API-equivalent list prices (USD per million tokens) for Antigravity
-- models (Gemini 4 family, Gemini 3.8 Flash, Orchestrator, GPT-OSS 120B).
-- Editable in Admin -> AI -> Prices (WP-2030).
INSERT OR IGNORE INTO model_prices(model, input, output, cache_write_5m, cache_write_1h, cache_read, updated_at) VALUES
    ('gemini-4-neon',           2.5,   15,    0, 0, 0.25,  0),
    ('gemini-4-hydrogen',       2.5,   15,    0, 0, 0.25,  0),
    ('gemini-4-carbon',         1.25,  10,    0, 0, 0.125, 0),
    ('gemini-4-argon',          1.25,  10,    0, 0, 0.125, 0),
    ('gemini-pro-orchestrator', 1.25,  10,    0, 0, 0.125, 0),
    ('gemini-3.8-flash',        0.3,   2.5,   0, 0, 0.03,  0),
    ('gpt-oss-120b',            0.2,   0.8,   0, 0, 0.02,  0);
