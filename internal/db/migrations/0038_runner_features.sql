-- What a runner's wp can do beyond the basics, as it reports at registration
-- (WP-6): "confine" means it confines a review's CLI to the checkout, so a
-- review job (a pull request's text in the prompt) is only ever given to one
-- that says so. A runner that predates this reports nothing.
ALTER TABLE runners ADD COLUMN features TEXT NOT NULL DEFAULT '[]';
