-- WP-439: rounds that make progress no longer count against a babysit's
-- limit; only the safety cap does, raised from 5 to 15. Running babysits
-- take the new cap.
UPDATE babysits SET max_rounds = 15 WHERE ended_at IS NULL AND max_rounds < 15;
